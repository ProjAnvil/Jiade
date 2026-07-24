package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
)

// RabbitPublisher publishes persistent, mandatory messages and considers an
// event delivered only after RabbitMQ positively confirms it.
type RabbitPublisher struct {
	channel       *amqp.Channel
	exchange      string
	confirmations <-chan amqp.Confirmation
	returns       <-chan amqp.Return
	mu            sync.Mutex
}

// NewRabbitPublisher enables confirms and return notifications on channel. A
// publisher serializes calls because confirms and returns are ordered per
// channel; use one publisher per relay worker for concurrency.
func NewRabbitPublisher(channel *amqp.Channel, exchange string) (*RabbitPublisher, error) {
	if channel == nil {
		return nil, errors.New("messaging rabbit channel is nil")
	}
	if err := channel.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable rabbit publisher confirmations: %w", err)
	}
	return &RabbitPublisher{
		channel:       channel,
		exchange:      exchange,
		confirmations: channel.NotifyPublish(make(chan amqp.Confirmation, 1)),
		returns:       channel.NotifyReturn(make(chan amqp.Return, 1)),
	}, nil
}

// Publish sends event using its type as the topic routing key. mandatory=true
// makes an unroutable message a failure instead of silently dropping it.
func (publisher *RabbitPublisher) Publish(ctx context.Context, event Event) error {
	if publisher == nil || publisher.channel == nil {
		return errors.New("messaging rabbit publisher is nil")
	}
	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	publisher.mu.Lock()
	defer publisher.mu.Unlock()
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
		return fmt.Errorf("publish event: %w", err)
	}
	return awaitAMQPPublishOutcome(ctx, publisher.confirmations, publisher.returns)
}

func awaitAMQPPublishOutcome(ctx context.Context, confirmations <-chan amqp.Confirmation, returns <-chan amqp.Return) error {
	select {
	case returnedMessage, ok := <-returns:
		if !ok {
			return errors.New("rabbit return notification closed")
		}
		return fmt.Errorf("rabbit returned mandatory message: %d %s", returnedMessage.ReplyCode, returnedMessage.ReplyText)
	default:
	}
	select {
	case returnedMessage, ok := <-returns:
		if !ok {
			return errors.New("rabbit return notification closed")
		}
		return fmt.Errorf("rabbit returned mandatory message: %d %s", returnedMessage.ReplyCode, returnedMessage.ReplyText)
	case confirmation, ok := <-confirmations:
		if !ok || !confirmation.Ack {
			return errors.New("rabbit negatively confirmed publish")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitPublishOutcome is separate from amqp.Channel so confirmation/return
// state transitions can be tested without a broker.
func awaitPublishOutcome(ctx context.Context, confirmations <-chan bool, returns <-chan error) error {
	select {
	case returned := <-returns:
		if returned != nil {
			return returned
		}
	default:
	}
	select {
	case returned := <-returns:
		if returned != nil {
			return returned
		}
		return errors.New("rabbit return notification closed")
	case confirmed := <-confirmations:
		if !confirmed {
			return errors.New("rabbit negatively confirmed publish")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
	if event.ID == "" || event.SchemaVersion != CurrentSchemaVersion || event.Type == "" || event.Subject == "" {
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
