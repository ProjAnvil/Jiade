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

// ConfirmedRouter publishes a replacement delivery and returns only after its
// broker outcome is known.
type ConfirmedRouter interface {
	Route(context.Context, string, string, amqp.Publishing) error
}

// RetryPolicy bounds transient delivery retries and gives retry and terminal
// failures distinct, confirmed broker destinations. A retry queue should expire
// messages back to the source queue so RabbitMQ maintains its x-death count.
type RetryPolicy struct {
	MaxAttempts          int
	Router               ConfirmedRouter
	RetryQueue           string
	RetryExchange        string
	RetryRoutingKey      string
	DeadLetterExchange   string
	DeadLetterRoutingKey string
}

// RabbitPublisher publishes persistent mandatory messages and waits for broker
// confirmation before reporting success.
type RabbitPublisher struct {
	channel       rabbitChannel
	exchange      string
	confirmations <-chan amqp.Confirmation
	returns       <-chan amqp.Return
	mu            sync.Mutex
	retired       bool
	watcherStop   chan struct{}
	watcherDone   chan struct{}
	channelClosed chan struct{}
	stopOnce      sync.Once
	signalOnce    sync.Once
	closeOnce     sync.Once
	closeErr      error
}

type rabbitChannel interface {
	Confirm(bool) error
	NotifyPublish(chan amqp.Confirmation) chan amqp.Confirmation
	NotifyReturn(chan amqp.Return) chan amqp.Return
	NotifyClose(chan *amqp.Error) chan *amqp.Error
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
	publisher := &RabbitPublisher{
		channel:       channel,
		exchange:      exchange,
		confirmations: channel.NotifyPublish(make(chan amqp.Confirmation, 1)),
		returns:       channel.NotifyReturn(make(chan amqp.Return, 1)),
		watcherStop:   make(chan struct{}),
		watcherDone:   make(chan struct{}),
		channelClosed: make(chan struct{}),
	}
	go publisher.watchClose(channel.NotifyClose(make(chan *amqp.Error, 1)))
	return publisher, nil
}

// Close retires the publisher, closes its channel once, and waits for its
// asynchronous close watcher.
func (publisher *RabbitPublisher) Close() error {
	if publisher == nil {
		return nil
	}
	publisher.mu.Lock()
	publisher.retired = true
	publisher.stopWatcher()
	publisher.closeOnce.Do(func() {
		publisher.closeErr = publisher.channel.Close()
	})
	closeErr := publisher.closeErr
	publisher.mu.Unlock()
	<-publisher.watcherDone
	return closeErr
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

	return publisher.Route(ctx, publisher.exchange, routingKey, amqp.Publishing{
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
}

// Route publishes a persistent mandatory message to an explicit destination.
// Every ambiguous or negative outcome retires the channel so a stale
// confirmation can never be consumed by a later publish.
func (publisher *RabbitPublisher) Route(
	ctx context.Context,
	exchange string,
	routingKey string,
	message amqp.Publishing,
) error {
	if publisher == nil || publisher.channel == nil {
		return errors.New("rabbit publisher is nil")
	}
	if routingKey == "" {
		return errors.New("rabbit routing key is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	message.DeliveryMode = amqp.Persistent

	publisher.mu.Lock()
	defer publisher.mu.Unlock()
	if publisher.retired {
		return errors.New("rabbit publisher channel is retired")
	}
	sequence := publisher.channel.GetNextPublishSeqNo()
	err := publisher.channel.PublishWithContext(ctx, exchange, routingKey, true, false, message)
	if err != nil {
		publisher.retireLocked()
		return fmt.Errorf("publish envelope: %w", err)
	}
	if err := publisher.awaitOutcome(ctx, sequence, message.MessageId); err != nil {
		publisher.retireLocked()
		return err
	}
	return nil
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
		case <-publisher.channelClosed:
			return errors.New("rabbit channel closed before publish outcome")
		}
	}
}

func (publisher *RabbitPublisher) retireLocked() {
	if publisher.retired {
		return
	}
	publisher.retired = true
	publisher.stopWatcher()
	publisher.closeOnce.Do(func() {
		publisher.closeErr = publisher.channel.Close()
	})
}

func (publisher *RabbitPublisher) watchClose(notifications <-chan *amqp.Error) {
	defer close(publisher.watcherDone)
	select {
	case <-notifications:
		publisher.signalOnce.Do(func() { close(publisher.channelClosed) })
		publisher.mu.Lock()
		publisher.retired = true
		publisher.mu.Unlock()
		publisher.stopWatcher()
	case <-publisher.watcherStop:
	}
}

func (publisher *RabbitPublisher) stopWatcher() {
	publisher.stopOnce.Do(func() { close(publisher.watcherStop) })
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
		return settleDelivery(ctx, errors.New("messaging transaction is nil"), consumer, delivery, policy)
	}

	var envelope Envelope
	if err := json.Unmarshal(delivery.Body, &envelope); err != nil {
		_ = tx.Rollback()
		return routeTerminal(ctx, fmt.Errorf("decode envelope: %w", err), delivery, policy)
	}
	if err := validateEnvelope(envelope); err != nil {
		_ = tx.Rollback()
		return routeTerminal(ctx, fmt.Errorf("invalid envelope: %w", err), delivery, policy)
	}
	if consumer == "" {
		_ = tx.Rollback()
		return routeTerminal(ctx, errors.New("messaging consumer is required"), delivery, policy)
	}
	if handler == nil {
		_ = tx.Rollback()
		return routeTerminal(ctx, errors.New("messaging handler is nil"), delivery, policy)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO inbox_message (consumer, message_id, message_type, processed_at)
		VALUES ($1, $2, $3, CURRENT_TIMESTAMP)
		ON CONFLICT (consumer, message_id) DO NOTHING`,
		consumer, envelope.MessageID, envelope.MessageType)
	if err != nil {
		_ = tx.Rollback()
		return settleDelivery(ctx, fmt.Errorf("insert Inbox message: %w", err), consumer, delivery, policy)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return settleDelivery(ctx, fmt.Errorf("read Inbox insert result: %w", err), consumer, delivery, policy)
	}
	if inserted > 0 {
		if err := handler(ctx, envelope); err != nil {
			_ = tx.Rollback()
			return settleDelivery(ctx, fmt.Errorf("handle message: %w", err), consumer, delivery, policy)
		}
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return settleDelivery(ctx, fmt.Errorf("commit message transaction: %w", err), consumer, delivery, policy)
	}
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("ack delivery: %w", err)
	}
	return nil
}

func settleDelivery(ctx context.Context, cause error, consumer string, delivery amqp.Delivery, policy RetryPolicy) error {
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	retryQueue := policy.RetryQueue
	if retryQueue == "" {
		retryQueue = consumer + ".retry"
	}
	if retryCount(delivery.Headers, retryQueue) >= maxAttempts {
		return routeTerminal(ctx, cause, delivery, policy)
	}
	return routeReplacement(ctx, cause, delivery, policy.Router, policy.RetryExchange, policy.RetryRoutingKey)
}

func routeTerminal(ctx context.Context, cause error, delivery amqp.Delivery, policy RetryPolicy) error {
	return routeReplacement(ctx, cause, delivery, policy.Router, policy.DeadLetterExchange, policy.DeadLetterRoutingKey)
}

func routeReplacement(
	ctx context.Context,
	cause error,
	delivery amqp.Delivery,
	router ConfirmedRouter,
	exchange string,
	routingKey string,
) error {
	if router == nil {
		routeErr := errors.New("confirmed routing dependency is required")
		return fmt.Errorf("route delivery to %s/%s: %w", exchange, routingKey, errors.Join(cause, routeErr))
	}
	if exchange == "" || routingKey == "" {
		routeErr := errors.New("routing exchange and key are required")
		return fmt.Errorf("route delivery to %s/%s: %w", exchange, routingKey, errors.Join(cause, routeErr))
	}
	message := amqp.Publishing{
		Headers:         cloneHeaders(delivery.Headers),
		ContentType:     delivery.ContentType,
		ContentEncoding: delivery.ContentEncoding,
		DeliveryMode:    amqp.Persistent,
		Priority:        delivery.Priority,
		CorrelationId:   delivery.CorrelationId,
		ReplyTo:         delivery.ReplyTo,
		Expiration:      delivery.Expiration,
		MessageId:       delivery.MessageId,
		Timestamp:       delivery.Timestamp,
		Type:            delivery.Type,
		AppId:           delivery.AppId,
		Body:            append([]byte(nil), delivery.Body...),
	}
	if err := router.Route(ctx, exchange, routingKey, message); err != nil {
		return fmt.Errorf("route delivery to %s/%s: %w", exchange, routingKey, errors.Join(cause, err))
	}
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("ack source after confirmed route to %s/%s: %w", exchange, routingKey, errors.Join(cause, err))
	}
	return cause
}

func cloneHeaders(headers amqp.Table) amqp.Table {
	if headers == nil {
		return nil
	}
	cloned := make(amqp.Table, len(headers))
	for key, value := range headers {
		cloned[key] = value
	}
	return cloned
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
