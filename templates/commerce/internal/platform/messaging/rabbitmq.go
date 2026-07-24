package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitPublisher publishes persistent, mandatory messages and considers an
// event delivered only after RabbitMQ positively confirms it.
type RabbitPublisher struct {
	channel       rabbitChannel
	exchange      string
	confirmations <-chan amqp.Confirmation
	returns       <-chan amqp.Return
	mu            sync.Mutex
	retired       bool
	available     atomic.Bool
	watcherStop   chan struct{}
	watcherDone   chan struct{}
	stopOnce      sync.Once
	closeOnce     sync.Once
	closeErr      error
}

type rabbitChannel interface {
	Confirm(noWait bool) error
	NotifyPublish(chan amqp.Confirmation) chan amqp.Confirmation
	NotifyReturn(chan amqp.Return) chan amqp.Return
	NotifyClose(chan *amqp.Error) chan *amqp.Error
	GetNextPublishSeqNo() uint64
	PublishWithContext(context.Context, string, string, bool, bool, amqp.Publishing) error
	Close() error
}

// NewRabbitPublisher enables confirms and return notifications on channel. A
// publisher serializes calls because confirms and returns are ordered per
// channel; use one publisher per relay worker for concurrency.
func NewRabbitPublisher(channel *amqp.Channel, exchange string) (*RabbitPublisher, error) {
	if channel == nil {
		return nil, fmt.Errorf("%w: rabbit channel is nil", ErrPublisherUnavailable)
	}
	return newRabbitPublisher(channel, exchange)
}

func newRabbitPublisher(channel rabbitChannel, exchange string) (*RabbitPublisher, error) {
	if channel == nil {
		return nil, fmt.Errorf("%w: rabbit channel is nil", ErrPublisherUnavailable)
	}
	if err := channel.Confirm(false); err != nil {
		return nil, fmt.Errorf("%w: enable rabbit publisher confirmations: %w", ErrPublisherUnavailable, err)
	}
	publisher := &RabbitPublisher{
		channel:       channel,
		exchange:      exchange,
		confirmations: channel.NotifyPublish(make(chan amqp.Confirmation, 1)),
		returns:       channel.NotifyReturn(make(chan amqp.Return, 1)),
		watcherStop:   make(chan struct{}),
		watcherDone:   make(chan struct{}),
	}
	publisher.available.Store(true)
	closeNotifications := channel.NotifyClose(make(chan *amqp.Error, 1))
	go publisher.watchClose(closeNotifications)
	return publisher, nil
}

// Available reports whether the publisher channel can accept new relay work.
// It is nonblocking so readiness checks cannot wait behind an in-flight publish.
func (publisher *RabbitPublisher) Available() bool {
	return publisher != nil && publisher.available.Load()
}

// Close retires the publisher, closes its channel once, and waits for the
// asynchronous close watcher to terminate.
func (publisher *RabbitPublisher) Close() error {
	if publisher == nil {
		return nil
	}
	publisher.mu.Lock()
	publisher.retired = true
	publisher.available.Store(false)
	publisher.stopWatcher()
	publisher.closeOnce.Do(func() {
		publisher.closeErr = publisher.channel.Close()
	})
	closeErr := publisher.closeErr
	publisher.mu.Unlock()
	<-publisher.watcherDone
	return closeErr
}

// Publish sends event using its type as the topic routing key. mandatory=true
// makes an unroutable message a failure instead of silently dropping it.
func (publisher *RabbitPublisher) Publish(ctx context.Context, event Event) error {
	if publisher == nil || publisher.channel == nil {
		return fmt.Errorf("%w: rabbit publisher is nil", ErrPublisherUnavailable)
	}
	if !validEventID(event.ID) {
		return errors.New("messaging event ID must be a UUID")
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.retired {
		return fmt.Errorf("%w: rabbit publisher channel is retired", ErrPublisherUnavailable)
	}
	sequence := publisher.channel.GetNextPublishSeqNo()
	if err := publisher.channel.PublishWithContext(ctx, publisher.exchange, event.Type, true, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		MessageId:    event.ID,
		Type:         event.Type,
		Timestamp:    event.OccurredAt,
		Headers: amqp.Table{
			"schema_version": event.SchemaVersion,
			"subject":        event.Subject,
			"correlation_id": event.CorrelationID,
			"causation_id":   event.CausationID,
		},
		Body: body,
	}); err != nil {
		publisher.retireLocked()
		return fmt.Errorf("%w: publish event: %w", ErrPublisherUnavailable, err)
	}
	err, aligned := awaitAMQPPublishOutcome(ctx, publisher.confirmations, publisher.returns, sequence, event.ID)
	if !aligned {
		publisher.retireLocked()
		return fmt.Errorf("%w: %w", ErrPublisherUnavailable, err)
	}
	return err
}

func (publisher *RabbitPublisher) retireLocked() {
	if publisher.retired {
		return
	}
	publisher.retired = true
	publisher.available.Store(false)
	publisher.stopWatcher()
	publisher.closeOnce.Do(func() {
		publisher.closeErr = publisher.channel.Close()
	})
}

func (publisher *RabbitPublisher) watchClose(notifications <-chan *amqp.Error) {
	defer close(publisher.watcherDone)
	select {
	case <-notifications:
		publisher.available.Store(false)
		publisher.mu.Lock()
		publisher.retired = true
		publisher.mu.Unlock()
		publisher.stopWatcher()
	case <-publisher.watcherStop:
	}
}

func (publisher *RabbitPublisher) stopWatcher() {
	publisher.stopOnce.Do(func() {
		close(publisher.watcherStop)
	})
}

func awaitAMQPPublishOutcome(
	ctx context.Context,
	confirmations <-chan amqp.Confirmation,
	returns <-chan amqp.Return,
	sequence uint64,
	messageID string,
) (error, bool) {
	var returnedErr error
	for {
		select {
		case returnedMessage, ok := <-returns:
			if !ok {
				return errors.New("rabbit return notification closed"), false
			}
			if returnedMessage.MessageId != messageID {
				return fmt.Errorf("rabbit return correlation lost: got message %q, want %q", returnedMessage.MessageId, messageID), false
			}
			returnedErr = fmt.Errorf("rabbit returned mandatory message: %d %s", returnedMessage.ReplyCode, returnedMessage.ReplyText)
		case confirmation, ok := <-confirmations:
			if !ok {
				return errors.New("rabbit confirmation notification closed"), false
			}
			if confirmation.DeliveryTag != sequence {
				return fmt.Errorf("rabbit confirmation correlation lost: got sequence %d, want %d", confirmation.DeliveryTag, sequence), false
			}
			if returnedErr == nil {
				select {
				case returnedMessage, ok := <-returns:
					if !ok {
						return errors.New("rabbit return notification closed"), false
					}
					if returnedMessage.MessageId != messageID {
						return fmt.Errorf("rabbit return correlation lost: got message %q, want %q", returnedMessage.MessageId, messageID), false
					}
					returnedErr = fmt.Errorf("rabbit returned mandatory message: %d %s", returnedMessage.ReplyCode, returnedMessage.ReplyText)
				default:
				}
			}
			if returnedErr != nil {
				return returnedErr, true
			}
			if !confirmation.Ack {
				return errors.New("rabbit negatively confirmed publish"), true
			}
			return nil, true
		case <-ctx.Done():
			return ctx.Err(), false
		}
	}
}

// AMQPDelivery adapts RabbitMQ's manual-ack delivery to Delivery. RetryCount
// reads x-death metadata produced by retry queues instead of using requeue.
type AMQPDelivery struct{ amqp.Delivery }

func (delivery AMQPDelivery) RetryCount() int {
	value, ok := delivery.Headers["x-death"]
	if !ok {
		return 0
	}
	entries, ok := value.([]interface{})
	if !ok {
		return 0
	}
	total := 0
	for _, entry := range entries {
		table, ok := entry.(amqp.Table)
		if !ok {
			continue
		}
		switch count := table["count"].(type) {
		case int64:
			total += int(count)
		case int32:
			total += int(count)
		case int:
			total += count
		}
	}
	return total
}

// ProcessRabbitDelivery decodes an envelope, runs it transactionally, then
// manually acknowledges only after commit. Invalid envelopes go straight DLQ.
func ProcessRabbitDelivery(ctx context.Context, tx pgx.Tx, consumer string, delivery amqp.Delivery, handler func(Event) error, policy RetryPolicy) error {
	var event Event
	if err := json.Unmarshal(delivery.Body, &event); err != nil {
		return rejectMalformed(ctx, tx, AMQPDelivery{Delivery: delivery}, fmt.Errorf("decode event envelope: %w", err))
	}
	if !validEventID(event.ID) || event.SchemaVersion != CurrentSchemaVersion || event.Type == "" || event.Subject == "" {
		return rejectMalformed(ctx, tx, AMQPDelivery{Delivery: delivery}, errors.New("invalid event envelope"))
	}
	if handler == nil {
		return rejectMalformed(ctx, tx, AMQPDelivery{Delivery: delivery}, errors.New("messaging rabbit handler is nil"))
	}
	return ProcessDelivery(ctx, tx, consumer, event, func() error { return handler(event) }, AMQPDelivery{Delivery: delivery}, policy)
}

func rejectMalformed(ctx context.Context, tx pgx.Tx, delivery Delivery, cause error) error {
	if tx != nil {
		_ = tx.Rollback(ctx)
	}
	if err := delivery.Reject(false); err != nil {
		return fmt.Errorf("reject malformed delivery after %v: %w", cause, err)
	}
	return NonRetryable(cause)
}
