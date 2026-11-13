package messaging

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// HandleOnce records an event for consumer in the caller's transaction before
// running handler. A duplicate consumer/event pair does not invoke handler.
// The caller must roll back the transaction when handler returns an error.
func HandleOnce(ctx context.Context, tx pgx.Tx, consumer string, event Event, handler func() error) error {
	if tx == nil {
		return errors.New("messaging inbox transaction is nil")
	}
	if consumer == "" {
		return errors.New("messaging inbox consumer is required")
	}
	if event.ID == "" {
		return errors.New("messaging event ID is required")
	}
	if !validEventID(event.ID) {
		return errors.New("messaging event ID must be a UUID")
	}
	if handler == nil {
		return errors.New("messaging inbox handler is required")
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO inbox_event (consumer, event_id, event_type)
		VALUES ($1, $2, $3)
		ON CONFLICT (consumer, event_id) DO NOTHING`, consumer, event.ID, event.Type)
	if err != nil {
		return fmt.Errorf("record inbox event: %w", err)
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	if err := handler(); err != nil {
		return fmt.Errorf("handle inbox event: %w", err)
	}
	return nil
}

// Delivery is the manual-ack subset used by ProcessDelivery. Retry attempts
// come from broker dead-letter metadata, not from in-memory process state.
type Delivery interface {
	RetryCount() int
	Ack(multiple bool) error
	Nack(multiple, requeue bool) error
	Reject(requeue bool) error
}

// RetryPolicy bounds transient retries. Retry queues are expected to be TTL
// queues with a dead-letter route back to the source queue.
type RetryPolicy struct {
	MaxAttempts int
}

// NonRetryable marks malformed or semantically invalid events for the DLQ.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetryableError{err: err}
}

type nonRetryableError struct{ err error }

func (err nonRetryableError) Error() string { return err.err.Error() }
func (err nonRetryableError) Unwrap() error { return err.err }

// ProcessDelivery invokes the inbox handler and commits it before manually
// acknowledging the broker delivery. Failures are never requeued directly:
// transient failures dead-letter into a bounded TTL retry route and terminal
// failures are rejected to the DLQ.
func ProcessDelivery(ctx context.Context, tx pgx.Tx, consumer string, event Event, handler func() error, delivery Delivery, policy RetryPolicy) error {
	if delivery == nil {
		return errors.New("messaging delivery is nil")
	}
	if tx == nil {
		return settleFailure(errors.New("messaging inbox transaction is nil"), delivery, policy)
	}
	if !validEventID(event.ID) {
		return rejectMalformed(ctx, tx, delivery, errors.New("messaging event ID must be a UUID"))
	}
	if err := HandleOnce(ctx, tx, consumer, event, handler); err != nil {
		_ = tx.Rollback(ctx)
		return settleFailure(err, delivery, policy)
	}
	if err := tx.Commit(ctx); err != nil {
		_ = tx.Rollback(ctx)
		return settleFailure(fmt.Errorf("commit inbox transaction: %w", err), delivery, policy)
	}
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("ack delivery: %w", err)
	}
	return nil
}

func settleFailure(cause error, delivery Delivery, policy RetryPolicy) error {
	if retryable(cause, delivery.RetryCount(), policy) {
		if err := delivery.Nack(false, false); err != nil {
			return fmt.Errorf("retry delivery after %v: %w", cause, err)
		}
		return cause
	}
	if err := delivery.Reject(false); err != nil {
		return fmt.Errorf("reject delivery after %v: %w", cause, err)
	}
	return cause
}

func retryable(err error, retryCount int, policy RetryPolicy) bool {
	var terminal nonRetryableError
	if errors.As(err, &terminal) {
		return false
	}
	maxAttempts := policy.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return retryCount < maxAttempts
}
