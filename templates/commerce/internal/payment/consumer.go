package payment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"commerce/internal/platform/messaging"
	amqp "github.com/rabbitmq/amqp091-go"
)

const paymentConsumer = "payment"

// Consumer applies order lifecycle events through the payment Service and the
// transactional Inbox. Each delivery is settled inside the inbox transaction;
// a duplicate event is a no-op.
type Consumer struct {
	store    *PostgresStore
	service  *Service
	policy   messaging.RetryPolicy
	retryQueue string
}

// DeliveryRouting captures the broker routes used to settle failed deliveries.
type DeliveryRouting struct {
	RetryExchange   string
	RetryRoutingKey string
	DeadExchange    string
	DeadRoutingKey  string
}

// NewConsumer binds a payment consumer to its store and service.
func NewConsumer(store *PostgresStore, service *Service, policy messaging.RetryPolicy) *Consumer {
	return &Consumer{store: store, service: service, policy: policy}
}

// WithRetryQueue records the retry queue name so x-death counts are read from
// the correct queue when computing retry attempts.
func (consumer *Consumer) WithRetryQueue(queue string) *Consumer {
	if consumer != nil {
		consumer.retryQueue = queue
	}
	return consumer
}

// ProcessDelivery applies a RabbitMQ delivery through the inbox + service in
// one transaction. Acknowledgement happens only after commit.
func (consumer *Consumer) ProcessDelivery(ctx context.Context, delivery amqp.Delivery) error {
	if consumer == nil || consumer.store == nil || consumer.store.pool == nil {
		return errors.New("payment consumer store is unavailable")
	}
	tx, err := consumer.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment delivery: %w", err)
	}
	return messaging.ProcessRabbitDeliveryForRetryQueue(ctx, tx, paymentConsumer, delivery,
		consumer.retryQueue, consumer.applyEvent, consumer.policy)
}

func (consumer *Consumer) applyEvent(event messaging.Event) error {
	switch event.Type {
	case "order.placed.v1":
		return consumer.applyOrderPlaced(event)
	case "payment.refund-requested.v1":
		return consumer.applyRefundRequested(event)
	case "order.cancelled.v1":
		return consumer.applyOrderCancelled(event)
	default:
		return messaging.NonRetryable(fmt.Errorf("unsupported payment event type %s", event.Type))
	}
}

func (consumer *Consumer) applyOrderPlaced(event messaging.Event) error {
	var payload struct {
		OrderID     string `json:"order_id"`
		Currency    string `json:"currency"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if err := decodePaymentEnvelope(event, &payload); err != nil {
		return err
	}
	if err := validatePaymentSubject(event, payload.OrderID); err != nil {
		return err
	}
	if payload.AmountMinor <= 0 || len(payload.Currency) != 3 {
		return messaging.NonRetryable(fmt.Errorf("invalid order.placed money %s/%d",
			payload.Currency, payload.AmountMinor))
	}
	_, err := consumer.service.CaptureOrder(context.Background(), CaptureCommand{
		OrderID:        payload.OrderID,
		Currency:       payload.Currency,
		AmountMinor:    payload.AmountMinor,
		IdempotencyKey: "place:" + payload.OrderID,
		CorrelationID:  event.CorrelationID,
		CausationID:    event.ID,
		OccurredAt:     event.OccurredAt,
	})
	return err
}

func (consumer *Consumer) applyRefundRequested(event messaging.Event) error {
	var payload struct {
		OrderID     string `json:"order_id"`
		Currency    string `json:"currency"`
		AmountMinor int64  `json:"amount_minor"`
	}
	if err := decodePaymentEnvelope(event, &payload); err != nil {
		return err
	}
	if err := validatePaymentSubject(event, payload.OrderID); err != nil {
		return err
	}
	if payload.AmountMinor <= 0 {
		return messaging.NonRetryable(errors.New("refund amount must be positive"))
	}
	_, err := consumer.service.Refund(context.Background(), RefundCommand{
		OrderID:        payload.OrderID,
		Currency:       payload.Currency,
		AmountMinor:    payload.AmountMinor,
		Reason:         event.Type,
		IdempotencyKey: "refund:" + payload.OrderID + ":" + event.ID,
		CorrelationID:  event.CorrelationID,
		CausationID:    event.ID,
		OccurredAt:     event.OccurredAt,
	})
	return err
}

func (consumer *Consumer) applyOrderCancelled(event messaging.Event) error {
	var payload struct {
		OrderID string `json:"order_id"`
		Reason  string `json:"reason"`
	}
	if err := decodePaymentEnvelope(event, &payload); err != nil {
		return err
	}
	if err := validatePaymentSubject(event, payload.OrderID); err != nil {
		return err
	}
	reason := payload.Reason
	if reason == "" {
		reason = "order.cancelled"
	}
	_, err := consumer.service.CancelIntent(context.Background(), CancelCommand{
		OrderID:        payload.OrderID,
		Reason:         reason,
		IdempotencyKey: "cancel:" + payload.OrderID,
		CorrelationID:  event.CorrelationID,
		CausationID:    event.ID,
		OccurredAt:     event.OccurredAt,
	})
	return err
}

func decodePaymentEnvelope(event messaging.Event, destination any) error {
	if event.SchemaVersion != messaging.CurrentSchemaVersion || event.Subject == "" ||
		event.OccurredAt.IsZero() || len(event.Data) == 0 || !json.Valid(event.Data) {
		return messaging.NonRetryable(fmt.Errorf("invalid %s envelope or payload", event.Type))
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return messaging.NonRetryable(fmt.Errorf("decode %s payload: %w", event.Type, err))
	}
	body, err := json.Marshal(destination)
	if err != nil || string(body) == "{}" {
		return messaging.NonRetryable(fmt.Errorf("empty %s payload", event.Type))
	}
	return nil
}

func validatePaymentSubject(event messaging.Event, orderID string) error {
	if orderID == "" || orderID != event.Subject {
		return messaging.NonRetryable(fmt.Errorf(
			"%s payload order_id %q does not match subject %q",
			event.Type, orderID, event.Subject))
	}
	return nil
}

// RunConsumer processes a manual-ack delivery stream until cancellation or a
// broker-side channel closure. Individual delivery failures are settled by
// ProcessDelivery and do not stop later deliveries.
func RunConsumer(
	ctx context.Context,
	channel *amqp.Channel,
	router confirmedPaymentRouter,
	queue string,
	consumer *Consumer,
	routing DeliveryRouting,
) error {
	if channel == nil {
		return errors.New("payment consumer channel is nil")
	}
	if consumer == nil {
		return errors.New("payment consumer is nil")
	}
	if router == nil {
		return errors.New("payment confirmed delivery router is nil")
	}
	if err := channel.Qos(20, 0, false); err != nil {
		return fmt.Errorf("configure payment consumer qos: %w", err)
	}
	deliveries, err := channel.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("start payment consumer: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, open := <-deliveries:
			if !open {
				if ctx.Err() != nil {
					return nil
				}
				return errors.New("payment consumer deliveries closed")
			}
			routed := delivery
			routed.Acknowledger = &retryAcknowledger{
				original: delivery.Acknowledger, publisher: router,
				delivery: delivery, routing: routing, ctx: ctx,
			}
			_ = consumer.ProcessDelivery(ctx, routed)
		}
	}
}

type confirmedPaymentRouter interface {
	Route(context.Context, string, string, amqp.Publishing) error
}

// retryAcknowledger routes transient Nack to the retry queue and terminal
// Reject to the DLQ, acknowledging the original only after the routed publish
// is confirmed by the channel.
type retryAcknowledger struct {
	original  amqp.Acknowledger
	publisher confirmedPaymentRouter
	delivery  amqp.Delivery
	routing   DeliveryRouting
	ctx       context.Context
}

func (acknowledger *retryAcknowledger) Ack(tag uint64, multiple bool) error {
	if acknowledger.original == nil {
		return errors.New("payment delivery acknowledger is unavailable")
	}
	return acknowledger.original.Ack(tag, multiple)
}

func (acknowledger *retryAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	if requeue || multiple {
		if acknowledger.original == nil {
			return errors.New("payment delivery acknowledger is unavailable")
		}
		return acknowledger.original.Nack(tag, multiple, requeue)
	}
	return acknowledger.publishThenAck(
		tag, acknowledger.routing.RetryExchange, acknowledger.routing.RetryRoutingKey)
}

func (acknowledger *retryAcknowledger) Reject(tag uint64, requeue bool) error {
	if requeue {
		if acknowledger.original == nil {
			return errors.New("payment delivery acknowledger is unavailable")
		}
		return acknowledger.original.Reject(tag, true)
	}
	return acknowledger.publishThenAck(
		tag, acknowledger.routing.DeadExchange, acknowledger.routing.DeadRoutingKey)
}

func (acknowledger *retryAcknowledger) publishThenAck(tag uint64, exchange, key string) error {
	if acknowledger.publisher == nil || acknowledger.original == nil ||
		exchange == "" || key == "" {
		return errors.New("payment delivery routing is unavailable")
	}
	ctx := acknowledger.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	delivery := acknowledger.delivery
	if err := acknowledger.publisher.Route(ctx, exchange, key, amqp.Publishing{
		Headers: delivery.Headers, ContentType: delivery.ContentType,
		ContentEncoding: delivery.ContentEncoding, DeliveryMode: amqp.Persistent,
		Priority: delivery.Priority, CorrelationId: delivery.CorrelationId,
		ReplyTo: delivery.ReplyTo, Expiration: delivery.Expiration,
		MessageId: delivery.MessageId, Timestamp: delivery.Timestamp,
		Type: delivery.Type, UserId: delivery.UserId, AppId: delivery.AppId,
		Body: delivery.Body,
	}); err != nil {
		return fmt.Errorf("route payment delivery to %s: %w", exchange, err)
	}
	if err := acknowledger.original.Ack(tag, false); err != nil {
		return fmt.Errorf("ack routed payment delivery: %w", err)
	}
	return nil
}

// WorkerLifecycle retains a worker's terminal result and supplies a reusable
// completion signal for process supervision and graceful shutdown.
type WorkerLifecycle struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

// NewWorkerLifecycle runs run in a goroutine and exposes its completion.
func NewWorkerLifecycle(ctx context.Context, run func(context.Context) error) *WorkerLifecycle {
	lifecycle := &WorkerLifecycle{done: make(chan struct{})}
	go func() {
		err := run(ctx)
		lifecycle.mu.Lock()
		lifecycle.err = err
		lifecycle.mu.Unlock()
		close(lifecycle.done)
	}()
	return lifecycle
}

// Done returns the channel closed when the worker terminates.
func (lifecycle *WorkerLifecycle) Done() <-chan struct{} { return lifecycle.done }

// Wait blocks until the worker terminates or ctx is cancelled.
func (lifecycle *WorkerLifecycle) Wait(ctx context.Context) error {
	if lifecycle == nil {
		return errors.New("payment worker is unavailable")
	}
	select {
	case <-lifecycle.done:
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		return lifecycle.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ErrIfStopped returns nil while the worker is running, or the terminal error
// (or a generic stopped sentinel) after it terminates.
func (lifecycle *WorkerLifecycle) ErrIfStopped() error {
	if lifecycle == nil {
		return errors.New("payment worker is unavailable")
	}
	select {
	case <-lifecycle.done:
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		if lifecycle.err != nil {
			return lifecycle.err
		}
		return errors.New("payment worker stopped")
	default:
		return nil
	}
}

type publisherAvailability interface {
	Available() bool
}

type combinedAvailability []publisherAvailability

func (publishers combinedAvailability) Available() bool {
	if len(publishers) == 0 {
		return false
	}
	for _, publisher := range publishers {
		if publisher == nil || !publisher.Available() {
			return false
		}
	}
	return true
}

// CombinePublisherAvailability returns a single readiness predicate over a set
// of publishers.
func CombinePublisherAvailability(publishers ...publisherAvailability) publisherAvailability {
	return combinedAvailability(publishers)
}

// NewRuntimeReadiness returns a readiness check over the database, publisher,
// broker, optional dependencies, and worker lifecycles.
func NewRuntimeReadiness(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	workers ...*WorkerLifecycle,
) func(context.Context) error {
	return NewRuntimeReadinessWithDependencies(database, publisher, brokerClosed, nil, workers...)
}

// NewRuntimeReadinessWithDependencies additionally probes a list of external
// dependency readiness functions before checking workers.
func NewRuntimeReadinessWithDependencies(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	dependencies []func(context.Context) error,
	workers ...*WorkerLifecycle,
) func(context.Context) error {
	return func(ctx context.Context) error {
		if database == nil {
			return errors.New("payment database readiness is unavailable")
		}
		if err := database(ctx); err != nil {
			return err
		}
		if brokerClosed != nil && brokerClosed() {
			return errors.New("payment broker connection is closed")
		}
		if publisher == nil || !publisher.Available() {
			return errors.New("payment publisher is unavailable")
		}
		for index, dependency := range dependencies {
			if dependency == nil {
				return fmt.Errorf("payment dependency %d readiness is unavailable", index)
			}
			if err := dependency(ctx); err != nil {
				return fmt.Errorf("payment dependency %d is unavailable: %w", index, err)
			}
		}
		if len(workers) == 0 {
			return errors.New("payment workers are unavailable")
		}
		for _, worker := range workers {
			if err := worker.ErrIfStopped(); err != nil {
				return fmt.Errorf("payment worker stopped: %w", err)
			}
		}
		return nil
	}
}
