// Package reward wires the reward service's read-only API and its
// payment-completion consumer.
//
// The Consumer reacts to payment.completed.v1 — emitted by the payment
// service when a payment-transfer workflow reaches StatusSucceeded — by
// earning reward points for the payer. It is a NON-CRITICAL consumer:
//
//   - Reward processing failures do NOT affect payment status. The reward
//     package does not import the payment package and has no access to the
//     payment_intent table. A permanently-failing reward delivery routes to
//     the reward DLQ while the payment remains succeeded.
//   - The consumer uses its own reward_db Inbox for at-least-once dedup and
//     its own RetryPolicy for retry/DLQ routing — both are fully decoupled
//     from the payment service's messaging infrastructure.
package reward

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"bank/internal/platform/messaging"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ConsumerName is the Inbox consumer identifier used by the reward consumer
// for at-least-once delivery dedup. It MUST be distinct from every other
// service's consumer so the Inbox rows do not collide.
const ConsumerName = "reward"

// EventTypePaymentCompleted is the message type the reward consumer
// subscribes to. The payment service's consumer emits this event when a
// payment-transfer workflow reaches StatusSucceeded (Task 8).
const EventTypePaymentCompleted = "payment.completed.v1"

// RoutePaymentCompleted is the broker routing key the payment outbox uses
// for payment.completed.v1; the reward queue binds to this key.
const RoutePaymentCompleted = "payment.completed"

// completionPayload mirrors the wire format of payment.completed.v1. The
// payment consumer populates these fields from the payment_intent row so the
// reward consumer does not need to call back into the payment service.
type completionPayload struct {
	WorkflowID      string `json:"workflow_id"`
	PaymentID       string `json:"payment_id"`
	PayerCustomerID string `json:"payer_customer_id"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
}

// PointsEarner is the reward-side write path invoked when a payment
// completes. The concrete implementation records a points-earn txn against
// the payer's points account. Returning an error causes ProcessDelivery to
// retry and eventually DLQ the delivery — payment status is never affected.
type PointsEarner interface {
	EarnPoints(ctx context.Context, paymentID, customerID string, amountMinor int64, currency string) error
}

// Consumer receives payment.completed.v1 deliveries and earns reward points.
// It is stateless between deliveries; all dedup happens in the Inbox and in
// the points-earn idempotency key (the PointsEarner implementation's
// responsibility).
type Consumer struct {
	db     *sql.DB
	earner PointsEarner
	policy messaging.RetryPolicy
}

// NewConsumer wires the points earner and the reward_db connection. db may be
// nil in unit tests that exercise processCompletion directly; it MUST be
// non-nil before Run is called. policy is the retry/DLQ settlement policy.
// The earner MUST be non-nil; a nil earner panics so a misconfigured service
// fails fast at wiring time.
func NewConsumer(db *sql.DB, earner PointsEarner, policy messaging.RetryPolicy) *Consumer {
	if earner == nil {
		panic("reward: NewConsumer requires a non-nil PointsEarner")
	}
	return &Consumer{db: db, earner: earner, policy: policy}
}

// processCompletion is the package-internal entry point tested by
// consumer_test.go. It decodes the completion payload and invokes the points
// earner. A decode failure or earner failure returns an error so
// messaging.ProcessDelivery retries/DLQs the delivery. It NEVER touches
// payment state — the reward package has no PaymentIntentRepo and no access
// to pay_db.
func (c *Consumer) processCompletion(ctx context.Context, env messaging.Envelope) error {
	var payload completionPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return fmt.Errorf("reward: decode %s payload: %w", env.MessageType, err)
	}
	if payload.PaymentID == "" {
		// Fall back to the envelope's workflow_id so a legacy payload
		// without payment_id still earns points.
		payload.PaymentID = env.WorkflowID
	}
	return c.earner.EarnPoints(ctx, payload.PaymentID, payload.PayerCustomerID, payload.AmountMinor, payload.Currency)
}

// ---------------------------------------------------------------------------
// AMQP runtime: Run subscribes to the completion queue and dispatches
// deliveries through messaging.ProcessDelivery (Inbox dedup + retry + DLQ).
// ---------------------------------------------------------------------------

// ConsumeDelivery is the AMQP entry point. It begins a reward_db tx, then
// delegates to messaging.ProcessDelivery which owns the Inbox insert, handler
// invocation, commit, and ack/retry/DLQ lifecycle. Mirrors the payment and
// corebanking consumers' ConsumeDelivery shape.
func (c *Consumer) ConsumeDelivery(ctx context.Context, delivery amqp.Delivery) error {
	if c.db == nil {
		return messaging.ProcessDelivery(ctx, nil, ConsumerName, delivery, c.handler(), c.policy)
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return messaging.ProcessDelivery(ctx, nil, ConsumerName, delivery, c.handler(), c.policy)
	}
	return messaging.ProcessDelivery(ctx, tx, ConsumerName, delivery, c.handler(), c.policy)
}

// handler returns the closure ProcessDelivery invokes once the Inbox row has
// been inserted (i.e. the delivery is not a duplicate). The closure decodes
// the envelope and calls processCompletion; any error propagates so
// ProcessDelivery retries/DLQs the delivery.
func (c *Consumer) handler() func(context.Context, messaging.Envelope) error {
	return func(ctx context.Context, env messaging.Envelope) error {
		return c.processCompletion(ctx, env)
	}
}

// Run connects to the RabbitMQ broker, subscribes to the completion queue,
// and dispatches each delivery to ConsumeDelivery. It blocks until ctx is
// cancelled (returns nil) or a fatal broker error occurs (returns error).
//
// The queue is bound to the payment.completed routing key by the seed or
// broker topology; Run only consumes. Set amqpURL/queue via environment
// variables in cmd/reward/main.go.
func (c *Consumer) Run(ctx context.Context, amqpURL, queue string) error {
	if c.db == nil {
		return errors.New("reward consumer: db is nil")
	}
	if amqpURL == "" {
		return errors.New("reward consumer: amqp URL is required")
	}
	if queue == "" {
		queue = ConsumerName + ".events"
	}

	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("reward consumer: dial broker: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("reward consumer: open channel: %w", err)
	}
	defer ch.Close()

	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("reward consumer: declare queue %s: %w", queue, err)
	}
	if err := ch.Qos(1, 0, false); err != nil {
		return fmt.Errorf("reward consumer: set QoS: %w", err)
	}

	publisher, err := messaging.NewRabbitPublisher(ch, "")
	if err != nil {
		return fmt.Errorf("reward consumer: retry publisher: %w", err)
	}
	c.policy.Router = publisher

	deliveries, err := ch.Consume(queue, ConsumerName, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("reward consumer: start consume: %w", err)
	}

	log.Printf("reward consumer: subscribed to queue %s", queue)
	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, ok := <-deliveries:
			if !ok {
				return errors.New("reward consumer: delivery channel closed")
			}
			if err := c.ConsumeDelivery(ctx, delivery); err != nil {
				log.Printf("reward consumer: delivery %s: %v", delivery.MessageId, err)
				// Non-fatal: retry/DLQ settlement is handled inside
				// ProcessDelivery. Continue processing subsequent deliveries.
			}
		}
	}
}
