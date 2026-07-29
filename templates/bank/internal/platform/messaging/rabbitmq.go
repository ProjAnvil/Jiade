package messaging

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

// RetryPolicy bounds transient delivery retries. Retry queues must dead-letter
// expired messages back to the source queue.
type RetryPolicy struct {
	MaxAttempts int
}

// RabbitPublisher publishes persistent mandatory messages and waits for broker
// confirmation before reporting success.
type RabbitPublisher struct {
	channel       rabbitChannel
	exchange      string
	confirmations <-chan amqp.Confirmation
	returns       <-chan amqp.Return
	mu            sync.Mutex
}

type rabbitChannel interface {
	Confirm(bool) error
	NotifyPublish(chan amqp.Confirmation) chan amqp.Confirmation
	NotifyReturn(chan amqp.Return) chan amqp.Return
	GetNextPublishSeqNo() uint64
	PublishWithContext(context.Context, string, string, bool, bool, amqp.Publishing) error
	Close() error
}

// NewRabbitPublisher enables publisher confirms for channel.
func NewRabbitPublisher(channel *amqp.Channel, exchange string) (*RabbitPublisher, error) {
	if channel == nil {
		return nil, errors.New("rabbit publisher channel is nil")
	}
	return newRabbitPublisher(channel, exchange)
}

func newRabbitPublisher(channel rabbitChannel, exchange string) (*RabbitPublisher, error) {
	if channel == nil {
		return nil, errors.New("rabbit publisher channel is nil")
	}
	if err := channel.Confirm(false); err != nil {
		return nil, fmt.Errorf("enable publisher confirms: %w", err)
	}
	return &RabbitPublisher{
		channel:       channel,
		exchange:      exchange,
		confirmations: channel.NotifyPublish(make(chan amqp.Confirmation, 1)),
		returns:       channel.NotifyReturn(make(chan amqp.Return, 1)),
	}, nil
}

// Publish publishes envelope to routingKey and returns only after RabbitMQ
// positively confirms it.
func (publisher *RabbitPublisher) Publish(ctx context.Context, routingKey string, envelope Envelope) error {
	if publisher == nil || publisher.channel == nil {
		return errors.New("rabbit publisher is nil")
	}
	if routingKey == "" {
		return errors.New("rabbit routing key is required")
	}
	if err := validateEnvelope(envelope); err != nil {
		return fmt.Errorf("invalid envelope: %w", err)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	sequence := publisher.channel.GetNextPublishSeqNo()
	err = publisher.channel.PublishWithContext(ctx, publisher.exchange, routingKey, true, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent,
		ContentType:  "application/json",
		MessageId:    envelope.MessageID,
		Type:         envelope.MessageType,
		Timestamp:    envelope.OccurredAt,
		Headers: amqp.Table{
			"schema_version": envelope.SchemaVersion,
			"correlation_id": envelope.CorrelationID,
			"causation_id":   envelope.CausationID,
		},
		Body: body,
	})
	if err != nil {
		return fmt.Errorf("publish envelope: %w", err)
	}
	return publisher.awaitOutcome(ctx, sequence, envelope.MessageID)
}

func (publisher *RabbitPublisher) awaitOutcome(ctx context.Context, sequence uint64, messageID string) error {
	var returned error
	for {
		select {
		case result, ok := <-publisher.returns:
			if !ok {
				return errors.New("rabbit return notification closed")
			}
			if result.MessageId != messageID {
				return fmt.Errorf("rabbit return correlation lost: got %q, want %q", result.MessageId, messageID)
			}
			returned = fmt.Errorf("rabbit returned mandatory message: %d %s", result.ReplyCode, result.ReplyText)
		case confirmation, ok := <-publisher.confirmations:
			if !ok {
				return errors.New("rabbit confirmation notification closed")
			}
			if confirmation.DeliveryTag != sequence {
				return fmt.Errorf("rabbit confirmation correlation lost: got %d, want %d", confirmation.DeliveryTag, sequence)
			}
			if returned == nil {
				select {
				case result := <-publisher.returns:
					if result.MessageId != messageID {
						return fmt.Errorf("rabbit return correlation lost: got %q, want %q", result.MessageId, messageID)
					}
					returned = fmt.Errorf("rabbit returned mandatory message: %d %s", result.ReplyCode, result.ReplyText)
				default:
				}
			}
			if returned != nil {
				return returned
			}
			if !confirmation.Ack {
				return errors.New("rabbit negatively confirmed publish")
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// ProcessDelivery owns tx from entry through terminal commit or rollback. The
// caller must not use tx after this function returns. A handler that performs
// domain mutation or Outbox work must capture this same tx lexically so Inbox,
// domain, and Outbox writes commit atomically.
//
// The sequence is validate/decode, insert Inbox, invoke a non-duplicate
// handler, commit, then acknowledge. Any pre-commit failure rolls back before
// bounded retry/DLQ settlement. Deliveries are never directly requeued.
func ProcessDelivery(
	ctx context.Context,
	tx *sql.Tx,
	consumer string,
	delivery amqp.Delivery,
	handler func(context.Context, Envelope) error,
	policy RetryPolicy,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if delivery.Acknowledger == nil {
		if tx != nil {
			_ = tx.Rollback()
		}
		return errors.New("rabbit delivery acknowledger is nil")
	}
	if tx == nil {
		return settleDelivery(errors.New("messaging transaction is nil"), consumer, delivery, policy)
	}

	var envelope Envelope
	if err := json.Unmarshal(delivery.Body, &envelope); err != nil {
		_ = tx.Rollback()
		return rejectDelivery(fmt.Errorf("decode envelope: %w", err), delivery)
	}
	if err := validateEnvelope(envelope); err != nil {
		_ = tx.Rollback()
		return rejectDelivery(fmt.Errorf("invalid envelope: %w", err), delivery)
	}
	if consumer == "" {
		_ = tx.Rollback()
		return rejectDelivery(errors.New("messaging consumer is required"), delivery)
	}
	if handler == nil {
		_ = tx.Rollback()
		return rejectDelivery(errors.New("messaging handler is nil"), delivery)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_message (consumer, message_id, message_type, processed_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT (consumer, message_id) DO NOTHING`,
		consumer, envelope.MessageID, envelope.MessageType)
	if err != nil {
		_ = tx.Rollback()
		return settleDelivery(fmt.Errorf("insert Inbox message: %w", err), consumer, delivery, policy)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return settleDelivery(fmt.Errorf("read Inbox insert result: %w", err), consumer, delivery, policy)
	}
	if inserted > 0 {
		if err := handler(ctx, envelope); err != nil {
			_ = tx.Rollback()
			return settleDelivery(fmt.Errorf("handle message: %w", err), consumer, delivery, policy)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return settleDelivery(fmt.Errorf("commit message transaction: %w", err), consumer, delivery, policy)
	}
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("ack delivery: %w", err)
	}
	return nil
}

func settleDelivery(cause error, consumer string, delivery amqp.Delivery, policy RetryPolicy) error {
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if retryCount(delivery.Headers, consumer+".retry") < maxAttempts {
		if err := delivery.Nack(false, false); err != nil {
			return fmt.Errorf("route delivery for retry after %v: %w", cause, err)
		}
		return cause
	}
	return rejectDelivery(cause, delivery)
}

func rejectDelivery(cause error, delivery amqp.Delivery) error {
	if err := delivery.Reject(false); err != nil {
		return fmt.Errorf("reject delivery after %v: %w", cause, err)
	}
	return cause
}

func retryCount(headers amqp.Table, retryQueue string) int {
	raw, ok := headers["x-death"]
	if !ok {
		return 0
	}
	entries, ok := raw.([]interface{})
	if !ok {
		return 0
	}
	total := 0
	for _, rawEntry := range entries {
		entry, ok := rawEntry.(amqp.Table)
		if !ok {
			continue
		}
		queue, _ := entry["queue"].(string)
		reason, _ := entry["reason"].(string)
		if retryQueue != "" {
			if queue != retryQueue {
				continue
			}
		} else if !strings.HasSuffix(queue, ".retry") {
			continue
		}
		if reason != "" && reason != "expired" {
			continue
		}
		switch count := entry["count"].(type) {
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
