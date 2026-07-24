package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Publisher confirms that an event has been accepted by the broker. It must
// return an error for negative confirmations and mandatory-message returns.
type Publisher interface {
	Publish(context.Context, Event) error
}

// RelayConfig controls transactional claims and polling. ClaimTTL permits an
// event to be resent after a process crashes after broker publish but before
// the published marker is stored, preserving at-least-once delivery.
type RelayConfig struct {
	BatchSize    int
	PollInterval time.Duration
	ClaimTTL     time.Duration
}

const (
	defaultRelayBatchSize = 100
	defaultRelayPoll      = time.Second
	defaultClaimTTL       = 30 * time.Second
)

// InsertOutbox records an event inside the domain transaction that produced
// it. The relay can only observe it after that transaction commits.
func InsertOutbox(ctx context.Context, tx pgx.Tx, event Event) error {
	if tx == nil {
		return errors.New("messaging outbox transaction is nil")
	}
	if event.ID == "" || event.Type == "" || event.Subject == "" || event.SchemaVersion <= 0 || len(event.Data) == 0 {
		return errors.New("messaging event is incomplete")
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO outbox_event (
			event_id, event_type, schema_version, subject, correlation_id,
			causation_id, occurred_at, payload
		) VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8)`,
		event.ID, event.Type, event.SchemaVersion, event.Subject, event.CorrelationID,
		event.CausationID, event.OccurredAt, event.Data)
	if err != nil {
		return fmt.Errorf("insert outbox event: %w", err)
	}
	return nil
}

// RunRelay claims pending rows in short database transactions and publishes
// after the claim commits. It returns nil when its context is cancelled.
func RunRelay(ctx context.Context, pool *pgxpool.Pool, publisher Publisher, config RelayConfig) error {
	if pool == nil {
		return errors.New("messaging outbox pool is nil")
	}
	if publisher == nil {
		return errors.New("messaging outbox publisher is nil")
	}
	config = normalizedRelayConfig(config)
	store := &postgresOutboxStore{pool: pool}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		claimed, err := relayOnce(ctx, store, publisher, config)
		if err != nil {
			return err
		}
		if claimed > 0 {
			continue
		}
		timer := time.NewTimer(config.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

type outboxClaim struct {
	Event Event
	token string
}

type outboxStore interface {
	Claim(context.Context, int, time.Duration) ([]outboxClaim, error)
	MarkPublished(context.Context, outboxClaim) error
	MarkFailed(context.Context, outboxClaim, error) error
}

func relayOnce(ctx context.Context, store outboxStore, publisher Publisher, config RelayConfig) (int, error) {
	if store == nil || publisher == nil {
		return 0, errors.New("messaging relay dependency is nil")
	}
	config = normalizedRelayConfig(config)
	claims, err := store.Claim(ctx, config.BatchSize, config.ClaimTTL)
	if err != nil {
		return 0, fmt.Errorf("claim outbox events: %w", err)
	}
	for _, claim := range claims {
		if err := publisher.Publish(ctx, claim.Event); err != nil {
			if markErr := store.MarkFailed(ctx, claim, err); markErr != nil {
				return 0, fmt.Errorf("record outbox publish failure: %w", markErr)
			}
			continue
		}
		if err := store.MarkPublished(ctx, claim); err != nil {
			return 0, fmt.Errorf("mark outbox event published: %w", err)
		}
	}
	return len(claims), nil
}

func normalizedRelayConfig(config RelayConfig) RelayConfig {
	if config.BatchSize <= 0 {
		config.BatchSize = defaultRelayBatchSize
	}
	if config.PollInterval <= 0 {
		config.PollInterval = defaultRelayPoll
	}
	if config.ClaimTTL <= 0 {
		config.ClaimTTL = defaultClaimTTL
	}
	return config
}

type postgresOutboxStore struct{ pool *pgxpool.Pool }

func (store *postgresOutboxStore) Claim(ctx context.Context, batchSize int, claimTTL time.Duration) ([]outboxClaim, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	token := newEventID()
	claimBefore := time.Now().UTC().Add(-claimTTL)
	rows, err := tx.Query(ctx, `
		WITH candidates AS (
			SELECT event_id
			FROM outbox_event
			WHERE published_at IS NULL
			  AND (claimed_at IS NULL OR claimed_at < $1)
			ORDER BY created_at, event_id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_event AS event
		SET claim_token = $3::uuid,
			claimed_at = now(),
			attempts = event.attempts + 1,
			last_error = NULL
		FROM candidates
		WHERE event.event_id = candidates.event_id
		RETURNING event.event_id, event.event_type, event.schema_version,
			event.subject, event.correlation_id, COALESCE(event.causation_id, ''),
			event.occurred_at, event.payload`, claimBefore, batchSize, token)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	claims := make([]outboxClaim, 0, batchSize)
	for rows.Next() {
		var event Event
		var data json.RawMessage
		if err := rows.Scan(&event.ID, &event.Type, &event.SchemaVersion, &event.Subject,
			&event.CorrelationID, &event.CausationID, &event.OccurredAt, &data); err != nil {
			return nil, err
		}
		event.Data = append(json.RawMessage(nil), data...)
		claims = append(claims, outboxClaim{Event: event, token: token})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return claims, nil
}

func (store *postgresOutboxStore) MarkPublished(ctx context.Context, claim outboxClaim) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE outbox_event
		SET published_at = now(), last_error = NULL
		WHERE event_id = $1 AND claim_token = $2::uuid AND published_at IS NULL`, claim.Event.ID, claim.token)
	return err
}

func (store *postgresOutboxStore) MarkFailed(ctx context.Context, claim outboxClaim, publishErr error) error {
	_, err := store.pool.Exec(ctx, `
		UPDATE outbox_event
		SET last_error = $3
		WHERE event_id = $1 AND claim_token = $2::uuid AND published_at IS NULL`, claim.Event.ID, claim.token, publishErr.Error())
	return err
}
