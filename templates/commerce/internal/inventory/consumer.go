package inventory

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

const inventoryConsumer = "inventory-saga"

// Consumer applies order saga events that request a reservation transition
// (commit / release) through the inventory Service and the transactional
// Inbox. Each delivery is settled inside the inbox transaction; a duplicate
// event is a no-op. This closes the inventory side of the saga loop so that
// order's inventory.commit-requested.v1 and inventory.release-requested.v1
// actually produce inventory.committed.v1 / inventory.released.v1.
//
// Order keeps its synchronous HTTP Release fast-path in internal/order/service.go
// for checkout-conflict compensation; the release-requested consumer here is the
// event-driven path and converges with the HTTP path because release is guarded
// by a terminal-state transition (idempotent).
type Consumer struct {
	store      *PostgresStore
	service    *Service
	policy     messaging.RetryPolicy
	retryQueue string
}

// DeliveryRouting captures the broker routes used to settle failed deliveries.
type DeliveryRouting struct {
	RetryExchange   string
	RetryRoutingKey string
	DeadExchange    string
	DeadRoutingKey  string
}

// NewConsumer binds an inventory consumer to its store and service.
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
		return errors.New("inventory consumer store is unavailable")
	}
	tx, err := consumer.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin inventory delivery: %w", err)
	}
	return messaging.ProcessRabbitDeliveryForRetryQueue(ctx, tx, inventoryConsumer, delivery,
		consumer.retryQueue, consumer.applyEvent, consumer.policy)
}

func (consumer *Consumer) applyEvent(event messaging.Event) error {
	switch event.Type {
	case "inventory.commit-requested.v1":
		return consumer.applyCommitRequested(event)
	case "inventory.release-requested.v1":
		return consumer.applyReleaseRequested(event)
	default:
		return messaging.NonRetryable(fmt.Errorf("unsupported inventory event type %s", event.Type))
	}
}

// orderIDEventPayload mirrors the exact shape order emits for
// inventory.commit-requested.v1 and inventory.release-requested.v1 in
// internal/order/store.go (insertDerivedEvents / requestInventoryRelease) so
// the strict DisallowUnknownFields decoder accepts it: just order_id.
type orderIDEventPayload struct {
	OrderID string `json:"order_id"`
}

func (consumer *Consumer) applyCommitRequested(event messaging.Event) error {
	var payload orderIDEventPayload
	if err := decodeInventoryEnvelope(event, &payload); err != nil {
		return err
	}
	if err := validateInventorySubject(event, payload.OrderID); err != nil {
		return err
	}
	// TransitionOrder(active -> committed) is idempotent: terminal == commit
	// makes a replay a no-op that returns nil without re-emitting the event,
	// and the inbox HandleOnce dedupes by event_id before that.
	_, err := consumer.service.TransitionOrder(context.Background(), payload.OrderID, ReservationCommit)
	return err
}

func (consumer *Consumer) applyReleaseRequested(event messaging.Event) error {
	var payload orderIDEventPayload
	if err := decodeInventoryEnvelope(event, &payload); err != nil {
		return err
	}
	if err := validateInventorySubject(event, payload.OrderID); err != nil {
		return err
	}
	// Idempotent: if the synchronous HTTP Release fast-path already released
	// the order, terminal == release and TransitionOrder returns (allocations,
	// false, nil) with no error and no re-emission. The order-side compensation
	// saga step is unblocked by the first release event regardless of path.
	_, err := consumer.service.TransitionOrder(context.Background(), payload.OrderID, ReservationRelease)
	return err
}

func decodeInventoryEnvelope(event messaging.Event, destination any) error {
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

func validateInventorySubject(event messaging.Event, orderID string) error {
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
	router confirmedInventoryRouter,
	queue string,
	consumer *Consumer,
	routing DeliveryRouting,
) error {
	if channel == nil {
		return errors.New("inventory consumer channel is nil")
	}
	if consumer == nil {
		return errors.New("inventory consumer is nil")
	}
	if router == nil {
		return errors.New("inventory confirmed delivery router is nil")
	}
	if err := channel.Qos(20, 0, false); err != nil {
		return fmt.Errorf("configure inventory consumer qos: %w", err)
	}
	deliveries, err := channel.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("start inventory consumer: %w", err)
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
				return errors.New("inventory consumer deliveries closed")
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

type confirmedInventoryRouter interface {
	Route(context.Context, string, string, amqp.Publishing) error
}

// retryAcknowledger routes transient Nack to the retry queue and terminal
// Reject to the DLQ, acknowledging the original only after the routed publish
// is confirmed by the channel.
type retryAcknowledger struct {
	original  amqp.Acknowledger
	publisher confirmedInventoryRouter
	delivery  amqp.Delivery
	routing   DeliveryRouting
	ctx       context.Context
}

func (acknowledger *retryAcknowledger) Ack(tag uint64, multiple bool) error {
	if acknowledger.original == nil {
		return errors.New("inventory delivery acknowledger is unavailable")
	}
	return acknowledger.original.Ack(tag, multiple)
}

func (acknowledger *retryAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	if requeue || multiple {
		if acknowledger.original == nil {
			return errors.New("inventory delivery acknowledger is unavailable")
		}
		return acknowledger.original.Nack(tag, multiple, requeue)
	}
	return acknowledger.publishThenAck(
		tag, acknowledger.routing.RetryExchange, acknowledger.routing.RetryRoutingKey)
}

func (acknowledger *retryAcknowledger) Reject(tag uint64, requeue bool) error {
	if requeue {
		if acknowledger.original == nil {
			return errors.New("inventory delivery acknowledger is unavailable")
		}
		return acknowledger.original.Reject(tag, true)
	}
	return acknowledger.publishThenAck(
		tag, acknowledger.routing.DeadExchange, acknowledger.routing.DeadRoutingKey)
}

func (acknowledger *retryAcknowledger) publishThenAck(tag uint64, exchange, key string) error {
	if acknowledger.publisher == nil || acknowledger.original == nil ||
		exchange == "" || key == "" {
		return errors.New("inventory delivery routing is unavailable")
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
		return fmt.Errorf("route inventory delivery to %s: %w", exchange, err)
	}
	if err := acknowledger.original.Ack(tag, false); err != nil {
		return fmt.Errorf("ack routed inventory delivery: %w", err)
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
		return errors.New("inventory worker is unavailable")
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
		return errors.New("inventory worker is unavailable")
	}
	select {
	case <-lifecycle.done:
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		if lifecycle.err != nil {
			return lifecycle.err
		}
		return errors.New("inventory worker stopped")
	default:
		return nil
	}
}
