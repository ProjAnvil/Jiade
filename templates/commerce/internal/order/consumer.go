package order

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"commerce/internal/platform/messaging"
	amqp "github.com/rabbitmq/amqp091-go"
)

const orderSagaConsumer = "order-saga"

// Consumer applies payment and fulfillment results through the transactional
// Inbox owned by PostgresStore.
type Consumer struct {
	store      *PostgresStore
	policy     messaging.RetryPolicy
	retryQueue string
}

type DeliveryRouting struct {
	RetryExchange   string
	RetryRoutingKey string
	DeadExchange    string
	DeadRoutingKey  string
}

func NewConsumer(store *PostgresStore, policy messaging.RetryPolicy) *Consumer {
	return &Consumer{store: store, policy: policy}
}

func (consumer *Consumer) WithRetryQueue(queue string) *Consumer {
	if consumer != nil {
		consumer.retryQueue = queue
	}
	return consumer
}

func (consumer *Consumer) Handle(ctx context.Context, event messaging.Event) error {
	if consumer == nil || consumer.store == nil {
		return errors.New("order consumer store is unavailable")
	}
	return consumer.store.HandleEvent(ctx, event)
}

// ProcessDelivery keeps the Inbox insert, domain transition, and derived
// Outbox rows in one transaction. The Task 3 helper acknowledges only after
// this transaction commits.
func (consumer *Consumer) ProcessDelivery(ctx context.Context, delivery amqp.Delivery) error {
	if consumer == nil || consumer.store == nil || consumer.store.pool == nil {
		return errors.New("order consumer store is unavailable")
	}
	tx, err := consumer.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin order delivery: %w", err)
	}
	return messaging.ProcessRabbitDeliveryForRetryQueue(ctx, tx, orderSagaConsumer, delivery, consumer.retryQueue,
		func(event messaging.Event) error {
			return consumer.store.applyEvent(ctx, tx, event)
		}, consumer.policy)
}

// RunConsumer processes a manual-ack delivery stream until cancellation or a
// broker-side channel closure. Individual delivery failures are settled by
// ProcessDelivery and do not stop later deliveries.
func RunConsumer(
	ctx context.Context,
	channel *amqp.Channel,
	router confirmedDeliveryRouter,
	queue string,
	consumer *Consumer,
	routing DeliveryRouting,
) error {
	if channel == nil {
		return errors.New("order consumer channel is nil")
	}
	if consumer == nil {
		return errors.New("order consumer is nil")
	}
	if router == nil {
		return errors.New("order confirmed delivery router is nil")
	}
	if err := channel.Qos(20, 0, false); err != nil {
		return fmt.Errorf("configure order consumer qos: %w", err)
	}
	deliveries, err := channel.Consume(queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("start order consumer: %w", err)
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
				return errors.New("order consumer deliveries closed")
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

type confirmedDeliveryRouter interface {
	Route(context.Context, string, string, amqp.Publishing) error
}

// retryAcknowledger gives Task 3's Nack and Reject settlements distinct broker
// routes. A transient Nack enters the TTL retry queue; a terminal Reject enters
// the DLQ. The original delivery is acknowledged only after the routed publish
// is accepted by the channel.
type retryAcknowledger struct {
	original  amqp.Acknowledger
	publisher confirmedDeliveryRouter
	delivery  amqp.Delivery
	routing   DeliveryRouting
	ctx       context.Context
}

func (acknowledger *retryAcknowledger) Ack(tag uint64, multiple bool) error {
	if acknowledger.original == nil {
		return errors.New("order delivery acknowledger is unavailable")
	}
	return acknowledger.original.Ack(tag, multiple)
}

func (acknowledger *retryAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	if requeue || multiple {
		if acknowledger.original == nil {
			return errors.New("order delivery acknowledger is unavailable")
		}
		return acknowledger.original.Nack(tag, multiple, requeue)
	}
	return acknowledger.publishThenAck(
		tag, acknowledger.routing.RetryExchange, acknowledger.routing.RetryRoutingKey)
}

func (acknowledger *retryAcknowledger) Reject(tag uint64, requeue bool) error {
	if requeue {
		if acknowledger.original == nil {
			return errors.New("order delivery acknowledger is unavailable")
		}
		return acknowledger.original.Reject(tag, true)
	}
	return acknowledger.publishThenAck(
		tag, acknowledger.routing.DeadExchange, acknowledger.routing.DeadRoutingKey)
}

func (acknowledger *retryAcknowledger) publishThenAck(tag uint64, exchange, key string) error {
	if acknowledger.publisher == nil || acknowledger.original == nil ||
		exchange == "" || key == "" {
		return errors.New("order delivery routing is unavailable")
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
		return fmt.Errorf("route order delivery to %s: %w", exchange, err)
	}
	if err := acknowledger.original.Ack(tag, false); err != nil {
		return fmt.Errorf("ack routed order delivery: %w", err)
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

func (lifecycle *WorkerLifecycle) Done() <-chan struct{} { return lifecycle.done }

func (lifecycle *WorkerLifecycle) Wait(ctx context.Context) error {
	if lifecycle == nil {
		return errors.New("order worker is unavailable")
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

func (lifecycle *WorkerLifecycle) ErrIfStopped() error {
	if lifecycle == nil {
		return errors.New("order worker is unavailable")
	}
	select {
	case <-lifecycle.done:
		lifecycle.mu.Lock()
		defer lifecycle.mu.Unlock()
		if lifecycle.err != nil {
			return lifecycle.err
		}
		return errors.New("order worker stopped")
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

func CombinePublisherAvailability(publishers ...publisherAvailability) publisherAvailability {
	return combinedAvailability(publishers)
}

func NewRuntimeReadiness(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	workers ...*WorkerLifecycle,
) func(context.Context) error {
	return newRuntimeReadiness(database, publisher, brokerClosed, nil, workers...)
}

func NewRuntimeReadinessWithDependencies(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	dependencies []func(context.Context) error,
	workers ...*WorkerLifecycle,
) func(context.Context) error {
	return newRuntimeReadiness(database, publisher, brokerClosed, dependencies, workers...)
}

func newRuntimeReadiness(
	database func(context.Context) error,
	publisher publisherAvailability,
	brokerClosed func() bool,
	dependencies []func(context.Context) error,
	workers ...*WorkerLifecycle,
) func(context.Context) error {
	return func(ctx context.Context) error {
		if database == nil {
			return errors.New("order database readiness is unavailable")
		}
		if err := database(ctx); err != nil {
			return err
		}
		if brokerClosed != nil && brokerClosed() {
			return errors.New("order broker connection is closed")
		}
		if publisher == nil || !publisher.Available() {
			return errors.New("order publisher is unavailable")
		}
		for index, dependency := range dependencies {
			if dependency == nil {
				return fmt.Errorf("order dependency %d readiness is unavailable", index)
			}
			if err := dependency(ctx); err != nil {
				return fmt.Errorf("order dependency %d is unavailable: %w", index, err)
			}
		}
		if len(workers) == 0 {
			return errors.New("order workers are unavailable")
		}
		for _, worker := range workers {
			if err := worker.ErrIfStopped(); err != nil {
				return fmt.Errorf("order worker stopped: %w", err)
			}
		}
		return nil
	}
}
