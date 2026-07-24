package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PaymentMethodSnapshot is the immutable view of the payment instrument stored
// alongside the intent. The schema requires method_type ∈ card/wallet/bank.
type PaymentMethodSnapshot struct {
	PaymentMethodID string          `json:"payment_method_id"`
	PaymentIntentID string          `json:"payment_intent_id"`
	MethodType      MethodType      `json:"method_type"`
	Network         string          `json:"network,omitempty"`
	LastFour        string          `json:"last_four,omitempty"`
	ExpiryMonth     int             `json:"expiry_month,omitempty"`
	ExpiryYear      int             `json:"expiry_year,omitempty"`
	BillingAddress  json.RawMessage `json:"billing_address,omitempty"`
}

// PostgresStore persists payment state in the dedicated payment database.
// All mutations run in a single transaction and the derived Outbox events are
// inserted alongside the domain rows so the relay only observes them post-commit.
type PostgresStore struct {
	pool  *pgxpool.Pool
	clock func() time.Time
}

// NewPostgresStore constructs a store bound to pool. clock defaults to time.Now.
func NewPostgresStore(pool *pgxpool.Pool, clock func() time.Time) *PostgresStore {
	if clock == nil {
		clock = time.Now
	}
	return &PostgresStore{pool: pool, clock: clock}
}

func (store *PostgresStore) assert() error {
	if store == nil || store.pool == nil {
		return errors.New("payment postgres store is unavailable")
	}
	return nil
}

func (store *PostgresStore) FindIntent(ctx context.Context, idempotencyKey string) (Intent, bool, error) {
	if err := store.assert(); err != nil {
		return Intent{}, false, err
	}
	intent, err := scanIntent(ctx, store.pool, `
		SELECT payment_intent_id, order_id, amount_minor, currency, status,
		       provider, COALESCE(provider_reference, ''), idempotency_key
		FROM payment_intent
		WHERE idempotency_key = $1`, idempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, false, nil
	}
	if err != nil {
		return Intent{}, false, fmt.Errorf("find payment intent: %w", err)
	}
	intent.RefundedMinor, err = store.sumRefunded(ctx, store.pool, intent.PaymentIntentID)
	if err != nil {
		return Intent{}, false, err
	}
	return intent, true, nil
}

func (store *PostgresStore) GetIntentByOrder(ctx context.Context, orderID string) (Intent, bool, error) {
	if err := store.assert(); err != nil {
		return Intent{}, false, err
	}
	intent, err := scanIntent(ctx, store.pool, `
		SELECT payment_intent_id, order_id, amount_minor, currency, status,
		       provider, COALESCE(provider_reference, ''), idempotency_key
		FROM payment_intent
		WHERE order_id = $1
		ORDER BY created_at DESC, payment_intent_id
		LIMIT 1`, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, false, nil
	}
	if err != nil {
		return Intent{}, false, fmt.Errorf("load payment intent by order: %w", err)
	}
	intent.RefundedMinor, err = store.sumRefunded(ctx, store.pool, intent.PaymentIntentID)
	if err != nil {
		return Intent{}, false, err
	}
	return intent, true, nil
}

func (store *PostgresStore) FindRefund(ctx context.Context, idempotencyKey string) (Refund, bool, error) {
	if err := store.assert(); err != nil {
		return Refund{}, false, err
	}
	refund, err := scanRefund(ctx, store.pool, `
		SELECT refund_id, payment_intent_id, amount_minor, status, reason, idempotency_key
		FROM refund
		WHERE idempotency_key = $1`, idempotencyKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Refund{}, false, nil
	}
	if err != nil {
		return Refund{}, false, fmt.Errorf("find refund: %w", err)
	}
	return refund, true, nil
}

func (store *PostgresStore) SaveCapture(ctx context.Context, outcome CaptureOutcome) (CaptureResult, error) {
	if err := store.assert(); err != nil {
		return CaptureResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CaptureResult{}, fmt.Errorf("begin payment capture: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	intent := outcome.Intent
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_intent (
			payment_intent_id, order_id, amount_minor, currency, status,
			provider, provider_reference, idempotency_key, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		intent.PaymentIntentID, intent.OrderID, intent.AmountMinor, intent.Currency,
		string(intent.Status), intent.Provider, intent.ProviderReference,
		intent.IdempotencyKey, store.clock().UTC()); err != nil {
		return CaptureResult{}, fmt.Errorf("insert payment intent: %w", err)
	}
	if err := insertPaymentMethod(ctx, tx, intent, defaultMethodType); err != nil {
		return CaptureResult{}, err
	}
	for _, attempt := range outcome.Attempts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO payment_attempt (
				attempt_id, payment_intent_id, status, failure_code, amount_minor, created_at
			) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6)
			ON CONFLICT (attempt_id) DO NOTHING`,
			attempt.AttemptID, attempt.PaymentIntentID, attempt.Status,
			string(attempt.FailureCode), attempt.AmountMinor, store.clock().UTC()); err != nil {
			return CaptureResult{}, fmt.Errorf("insert payment attempt: %w", err)
		}
	}
	for _, event := range outcome.Events {
		if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
			return CaptureResult{}, fmt.Errorf("insert capture outbox: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CaptureResult{}, fmt.Errorf("commit payment capture: %w", err)
	}
	return CaptureResult{Intent: intent, Attempts: outcome.Attempts, Events: outcome.Events}, nil
}

func (store *PostgresStore) SaveRefund(ctx context.Context, outcome RefundOutcome) (RefundResult, error) {
	if err := store.assert(); err != nil {
		return RefundResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return RefundResult{}, fmt.Errorf("begin payment refund: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	intent := outcome.Intent
	if _, err := tx.Exec(ctx, `
		UPDATE payment_intent
		SET status = $2
		WHERE payment_intent_id = $1`,
		intent.PaymentIntentID, string(intent.Status)); err != nil {
		return RefundResult{}, fmt.Errorf("update refunded intent: %w", err)
	}
	refund := outcome.Refund
	if _, err := tx.Exec(ctx, `
		INSERT INTO refund (
			refund_id, payment_intent_id, amount_minor, status, reason,
			idempotency_key, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		refund.RefundID, refund.PaymentIntentID, refund.AmountMinor,
		refund.Status, refund.Reason, refund.IdempotencyKey, store.clock().UTC()); err != nil {
		if uniqueViolation(err) {
			return RefundResult{}, fmt.Errorf("duplicate refund: %w", err)
		}
		return RefundResult{}, fmt.Errorf("insert refund: %w", err)
	}
	for _, event := range outcome.Events {
		if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
			return RefundResult{}, fmt.Errorf("insert refund outbox: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RefundResult{}, fmt.Errorf("commit payment refund: %w", err)
	}
	return RefundResult{Intent: intent, Refund: refund, Events: outcome.Events}, nil
}

func (store *PostgresStore) SaveCancel(ctx context.Context, outcome CancelOutcome) (CancelResult, error) {
	if err := store.assert(); err != nil {
		return CancelResult{}, err
	}
	intent := outcome.Intent
	if _, err := store.pool.Exec(ctx, `
		UPDATE payment_intent
		SET status = $2
		WHERE payment_intent_id = $1 AND status <> $2`,
		intent.PaymentIntentID, string(intent.Status)); err != nil {
		return CancelResult{}, fmt.Errorf("cancel payment intent: %w", err)
	}
	return CancelResult{Intent: intent, Events: outcome.Events}, nil
}

// ListAttempts loads the attempts recorded for intentID, ordered chronologically.
func (store *PostgresStore) ListAttempts(ctx context.Context, intentID string) ([]Attempt, error) {
	if err := store.assert(); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT attempt_id, payment_intent_id, status,
		       COALESCE(failure_code, ''), amount_minor
		FROM payment_attempt
		WHERE payment_intent_id = $1
		ORDER BY created_at, attempt_id`, intentID)
	if err != nil {
		return nil, fmt.Errorf("list payment attempts: %w", err)
	}
	defer rows.Close()
	attempts := make([]Attempt, 0)
	for rows.Next() {
		var attempt Attempt
		var code string
		if err := rows.Scan(&attempt.AttemptID, &attempt.PaymentIntentID,
			&attempt.Status, &code, &attempt.AmountMinor); err != nil {
			return nil, fmt.Errorf("scan payment attempt: %w", err)
		}
		attempt.FailureCode = FailureCode(code)
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate payment attempts: %w", err)
	}
	return attempts, nil
}

// ListRefunds loads the refunds recorded for intentID, ordered chronologically.
func (store *PostgresStore) ListRefunds(ctx context.Context, intentID string) ([]Refund, error) {
	if err := store.assert(); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT refund_id, payment_intent_id, amount_minor, status, reason, idempotency_key
		FROM refund
		WHERE payment_intent_id = $1
		ORDER BY created_at, refund_id`, intentID)
	if err != nil {
		return nil, fmt.Errorf("list refunds: %w", err)
	}
	defer rows.Close()
	refunds := make([]Refund, 0)
	for rows.Next() {
		var refund Refund
		if err := rows.Scan(&refund.RefundID, &refund.PaymentIntentID,
			&refund.AmountMinor, &refund.Status, &refund.Reason,
			&refund.IdempotencyKey); err != nil {
			return nil, fmt.Errorf("scan refund: %w", err)
		}
		refunds = append(refunds, refund)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refunds: %w", err)
	}
	return refunds, nil
}

func (store *PostgresStore) sumRefunded(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, intentID string) (int64, error) {
	var total int64
	if err := queryer.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_minor), 0)
		FROM refund
		WHERE payment_intent_id = $1 AND status = 'succeeded'`, intentID).Scan(&total); err != nil {
		return 0, fmt.Errorf("sum refunded: %w", err)
	}
	return total, nil
}

func scanIntent(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, query string, args ...any) (Intent, error) {
	var intent Intent
	var status, provider string
	err := queryer.QueryRow(ctx, query, args...).Scan(
		&intent.PaymentIntentID, &intent.OrderID, &intent.AmountMinor,
		&intent.Currency, &status, &provider, &intent.ProviderReference,
		&intent.IdempotencyKey)
	intent.Status = State(status)
	intent.Provider = provider
	return intent, err
}

func scanRefund(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, query string, args ...any) (Refund, error) {
	var refund Refund
	err := queryer.QueryRow(ctx, query, args...).Scan(
		&refund.RefundID, &refund.PaymentIntentID, &refund.AmountMinor,
		&refund.Status, &refund.Reason, &refund.IdempotencyKey)
	return refund, err
}

func insertPaymentMethod(ctx context.Context, tx pgx.Tx, intent Intent, methodType MethodType) error {
	methodID := deterministicMethodID(intent.PaymentIntentID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO payment_method_snapshot (
			payment_method_id, payment_intent_id, method_type, created_at
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (payment_method_id) DO NOTHING`,
		methodID, intent.PaymentIntentID, string(methodType), time.Now().UTC()); err != nil {
		return fmt.Errorf("insert payment method: %w", err)
	}
	return nil
}

func deterministicMethodID(intentID string) string {
	sum := sha256.Sum256([]byte("payment_method\x00" + intentID))
	return "pm_" + hex.EncodeToString(sum[:12])
}

func simpleHash(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:12])
}

func uniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

var _ Store = (*PostgresStore)(nil)
