//go:build integration

package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresCaptureIsAtomicAndIdempotent(t *testing.T) {
	store, pool := newIntegrationPaymentStore(t)
	ctx := context.Background()
	intentID := deterministicIntentID("place-int-1")
	now := integrationPaymentClock()
	intent := Intent{
		PaymentIntentID: intentID, OrderID: "ORD-INT-1", AmountMinor: 2500,
		Currency: "CNY", Status: StateSucceeded, Provider: defaultProvider,
		ProviderReference: "pr_ref_1", IdempotencyKey: "place-int-1",
	}
	attempts := []Attempt{
		{AttemptID: deterministicAttemptID(intentID, 1), PaymentIntentID: intentID,
			Status: "failed", FailureCode: FailureProviderTimeout, AmountMinor: 2500},
		{AttemptID: deterministicAttemptID(intentID, 2), PaymentIntentID: intentID,
			Status: "succeeded", AmountMinor: 2500},
	}
	captured := messaging.NewEvent(EventPaymentCaptured, intent.OrderID, "corr-1", "",
		mustPaymentJSON(moneyResultPayload{OrderID: intent.OrderID, Currency: "CNY", AmountMinor: 2500}),
		func() time.Time { return now })
	outcome := CaptureOutcome{Intent: intent, Attempts: attempts, Events: []messaging.Event{captured}}
	first, err := store.SaveCapture(ctx, outcome)
	if err != nil {
		t.Fatalf("SaveCapture error: %v", err)
	}
	if first.Intent.Status != StateSucceeded {
		t.Fatalf("status=%q, want succeeded", first.Intent.Status)
	}
	// Replay must not duplicate rows or raise an error.
	_, err = store.SaveCapture(ctx, outcome)
	if err != nil {
		t.Fatalf("replay SaveCapture error: %v", err)
	}
	assertPaymentCount(t, pool, "payment_intent", 1)
	assertPaymentCount(t, pool, "payment_method_snapshot", 1)
	assertPaymentCount(t, pool, "payment_attempt", 2)
	assertPaymentCount(t, pool, "outbox_event", 1)

	loaded, found, err := store.GetIntentByOrder(ctx, "ORD-INT-1")
	if err != nil {
		t.Fatalf("GetIntentByOrder error: %v", err)
	}
	if !found || loaded.PaymentIntentID != intentID || loaded.Status != StateSucceeded {
		t.Fatalf("loaded=%+v found=%v", loaded, found)
	}
}

func TestPostgresRefundAccumulatesAndTransitionsStatus(t *testing.T) {
	store, pool := newIntegrationPaymentStore(t)
	ctx := context.Background()
	intentID := deterministicIntentID("place-int-2")
	now := integrationPaymentClock()
	intent := Intent{
		PaymentIntentID: intentID, OrderID: "ORD-INT-2", AmountMinor: 3000,
		Currency: "CNY", Status: StateSucceeded, Provider: defaultProvider,
		ProviderReference: "pr_ref_2", IdempotencyKey: "place-int-2",
	}
	captured := messaging.NewEvent(EventPaymentCaptured, intent.OrderID, "corr-2", "",
		mustPaymentJSON(moneyResultPayload{OrderID: intent.OrderID, Currency: "CNY", AmountMinor: 3000}),
		func() time.Time { return now })
	if _, err := store.SaveCapture(ctx, CaptureOutcome{
		Intent: intent, Attempts: []Attempt{{
			AttemptID: deterministicAttemptID(intentID, 1), PaymentIntentID: intentID,
			Status: "succeeded", AmountMinor: 3000,
		}}, Events: []messaging.Event{captured},
	}); err != nil {
		t.Fatal(err)
	}
	intent.Status = StatePartiallyRefunded
	intent.RefundedMinor = 1000
	refund := Refund{
		RefundID: deterministicRefundID(intentID, "refund-int-2a"),
		PaymentIntentID: intentID, AmountMinor: 1000, Status: "succeeded",
		Reason: "partial", IdempotencyKey: "refund-int-2a",
	}
	refundEvent := messaging.NewEvent(EventRefundSucceeded, intent.OrderID, "corr-2", "",
		mustPaymentJSON(moneyResultPayload{OrderID: intent.OrderID, Currency: "CNY", AmountMinor: 1000}),
		func() time.Time { return now })
	if _, err := store.SaveRefund(ctx, RefundOutcome{
		Intent: intent, Refund: refund, Events: []messaging.Event{refundEvent},
	}); err != nil {
		t.Fatalf("SaveRefund error: %v", err)
	}
	loaded, _, err := store.GetIntentByOrder(ctx, "ORD-INT-2")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StatePartiallyRefunded || loaded.RefundedMinor != 1000 {
		t.Fatalf("after partial refund loaded=%+v", loaded)
	}
	intent.Status = StateRefunded
	intent.RefundedMinor = 3000
	fullRefund := Refund{
		RefundID: deterministicRefundID(intentID, "refund-int-2b"),
		PaymentIntentID: intentID, AmountMinor: 2000, Status: "succeeded",
		Reason: "remaining", IdempotencyKey: "refund-int-2b",
	}
	fullEvent := messaging.NewEvent(EventRefundSucceeded, intent.OrderID, "corr-2", "",
		mustPaymentJSON(moneyResultPayload{OrderID: intent.OrderID, Currency: "CNY", AmountMinor: 2000}),
		func() time.Time { return now })
	if _, err := store.SaveRefund(ctx, RefundOutcome{
		Intent: intent, Refund: fullRefund, Events: []messaging.Event{fullEvent},
	}); err != nil {
		t.Fatalf("SaveRefund full error: %v", err)
	}
	loaded, _, err = store.GetIntentByOrder(ctx, "ORD-INT-2")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != StateRefunded || loaded.RefundedMinor != 3000 {
		t.Fatalf("after full refund loaded=%+v", loaded)
	}
	assertPaymentCount(t, pool, "refund", 2)
}

func TestPostgresSchemaRejectsAttemptExceedingIntentAmount(t *testing.T) {
	store, pool := newIntegrationPaymentStore(t)
	ctx := context.Background()
	intentID := deterministicIntentID("place-int-3")
	intent := Intent{
		PaymentIntentID: intentID, OrderID: "ORD-INT-3", AmountMinor: 500,
		Currency: "CNY", Status: StateSucceeded, Provider: defaultProvider,
		IdempotencyKey: "place-int-3",
	}
	captured := messaging.NewEvent(EventPaymentCaptured, intent.OrderID, "corr", "",
		mustPaymentJSON(moneyResultPayload{OrderID: intent.OrderID, Currency: "CNY", AmountMinor: 500}),
		func() time.Time { return integrationPaymentClock() })
	if _, err := store.SaveCapture(ctx, CaptureOutcome{
		Intent: intent, Attempts: []Attempt{{
			AttemptID: deterministicAttemptID(intentID, 1), PaymentIntentID: intentID,
			Status: "succeeded", AmountMinor: 500,
		}}, Events: []messaging.Event{captured},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_attempt (
			attempt_id, payment_intent_id, status, amount_minor, created_at
		) VALUES ('att_overflow', $1, 'succeeded', 99999, $2)`,
		intentID, integrationPaymentClock()); err == nil {
		t.Fatal("schema accepted attempt amount exceeding intent amount")
	}
}

func TestPostgresRefundBeforeCaptureIsRejectedByTrigger(t *testing.T) {
	store, pool := newIntegrationPaymentStore(t)
	ctx := context.Background()
	intentID := deterministicIntentID("place-int-4")
	intent := Intent{
		PaymentIntentID: intentID, OrderID: "ORD-INT-4", AmountMinor: 800,
		Currency: "CNY", Status: StateProcessing, Provider: defaultProvider,
		IdempotencyKey: "place-int-4",
	}
	captured := messaging.NewEvent(EventPaymentCaptured, intent.OrderID, "corr", "",
		mustPaymentJSON(moneyResultPayload{OrderID: intent.OrderID, Currency: "CNY", AmountMinor: 800}),
		func() time.Time { return integrationPaymentClock() })
	if _, err := store.SaveCapture(ctx, CaptureOutcome{
		Intent: intent, Attempts: []Attempt{{
			AttemptID: deterministicAttemptID(intentID, 1), PaymentIntentID: intentID,
			Status: "processing", AmountMinor: 800,
		}}, Events: []messaging.Event{captured},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO refund (
			refund_id, payment_intent_id, amount_minor, status, reason,
			idempotency_key, created_at
		) VALUES ('rfd_early', $1, 800, 'pending', 'early', 'refund-int-4', $2)`,
		intentID, integrationPaymentClock()); err == nil {
		t.Fatal("schema accepted refund on non-captured intent")
	}
}

func integrationPaymentClock() time.Time {
	return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
}

func mustPaymentJSON(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return body
}

func assertPaymentCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Fatalf("%s count=%d, want %d", table, count, want)
	}
}

func newIntegrationPaymentStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("payment integration test skipped: TEST_DATABASE_URL for a dedicated PostgreSQL database is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open dedicated TEST_DATABASE_URL: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("dedicated PostgreSQL at TEST_DATABASE_URL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("task7a_payment_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "payment_db.sql"))
	if err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop integration schema: %v", err)
		}
		admin.Close()
	})
	return NewPostgresStore(pool, func() time.Time { return integrationPaymentClock() }), pool
}

var _ = errors.Is
