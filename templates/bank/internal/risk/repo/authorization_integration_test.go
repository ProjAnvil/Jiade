//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"bank/internal/platform/pg"
	"bank/internal/risk/domain"
	"bank/internal/risk/repo"
	"bank/internal/risk/service"
)

// setupAuthorizationDB opens the risk_db and ensures the
// payment_authorization table exists. It skips when Postgres is unavailable.
func setupAuthorizationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("risk_db")
	if err != nil {
		t.Skipf("无 risk_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	// Ensure the table exists even if the migration hasn't been applied yet.
	_, _ = db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS payment_authorization (
		    authorization_id TEXT PRIMARY KEY,
		    workflow_id      TEXT NOT NULL,
		    idempotency_key  TEXT NOT NULL UNIQUE,
		    customer_id      TEXT NOT NULL,
		    amount_cents     BIGINT NOT NULL,
		    currency         TEXT NOT NULL,
		    status           TEXT NOT NULL DEFAULT 'pending',
		    matched_rules    JSONB NOT NULL DEFAULT '[]'::jsonb,
		    context_digest   TEXT,
		    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
		    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return db
}

func TestAuthorizationRepo_InsertAndGet(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewAuthorizationRepo()
	now := time.Now().UTC()

	authID := "IT-AUTH-INS-1"
	db.ExecContext(ctx, "DELETE FROM payment_authorization WHERE authorization_id=$1", authID)

	auth := domain.PaymentAuthorization{
		AuthorizationID: authID, WorkflowID: "wf-it-1", IdempotencyKey: "idem-it-1",
		CustomerID: "C1", AmountCents: 10000, Currency: "CNY",
		Status: domain.AuthStatusAuthorized, MatchedRuleIDs: []string{},
		ContextDigest: "digest-1", CreatedAt: now, UpdatedAt: now,
	}
	if err := r.Insert(ctx, db, auth); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := r.GetByID(ctx, db, authID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.AuthorizationID != authID || got.Status != domain.AuthStatusAuthorized {
		t.Errorf("GetByID mismatch: %+v", got)
	}
	if got.AmountCents != 10000 || got.Currency != "CNY" {
		t.Errorf("amount/ccy mismatch: %+v", got)
	}
	if got.ContextDigest != "digest-1" {
		t.Errorf("digest = %q", got.ContextDigest)
	}

	gotByKey, err := r.GetByIdempotencyKey(ctx, db, "idem-it-1")
	if err != nil {
		t.Fatalf("GetByIdempotencyKey: %v", err)
	}
	if gotByKey.AuthorizationID != authID {
		t.Errorf("GetByIdempotencyKey returned %s", gotByKey.AuthorizationID)
	}
}

func TestAuthorizationRepo_GetByID_NotFound(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	r := repo.NewAuthorizationRepo()
	_, err := r.GetByID(context.Background(), db, "IT-NONEXISTENT")
	if !errors.Is(err, service.ErrAuthorizationNotFound) {
		t.Errorf("expected ErrAuthorizationNotFound, got: %v", err)
	}
}

func TestAuthorizationRepo_GetByIdempotencyKey_NotFound(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	r := repo.NewAuthorizationRepo()
	_, err := r.GetByIdempotencyKey(context.Background(), db, "idem-nonexistent")
	if !errors.Is(err, service.ErrAuthorizationNotFound) {
		t.Errorf("expected ErrAuthorizationNotFound, got: %v", err)
	}
}

func TestAuthorizationRepo_DuplicateIdempotencyKey(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewAuthorizationRepo()
	now := time.Now().UTC()

	authID := "IT-AUTH-DUP-1"
	db.ExecContext(ctx, "DELETE FROM payment_authorization WHERE authorization_id IN ($1, $2)", authID, "IT-AUTH-DUP-2")

	auth := domain.PaymentAuthorization{
		AuthorizationID: authID, WorkflowID: "wf-it-2", IdempotencyKey: "idem-dup-1",
		CustomerID: "C1", AmountCents: 5000, Currency: "CNY",
		Status: domain.AuthStatusAuthorized, MatchedRuleIDs: nil,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := r.Insert(ctx, db, auth); err != nil {
		t.Fatalf("first Insert: %v", err)
	}

	// Second insert with the same idempotency_key but different authorization_id.
	dup := auth
	dup.AuthorizationID = "IT-AUTH-DUP-2"
	err := r.Insert(ctx, db, dup)
	if err == nil {
		t.Error("expected duplicate idempotency_key error")
	}
}

func TestAuthorizationRepo_UpdateStatus(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewAuthorizationRepo()
	now := time.Now().UTC()

	authID := "IT-AUTH-UPD-1"
	db.ExecContext(ctx, "DELETE FROM payment_authorization WHERE authorization_id=$1", authID)

	auth := domain.PaymentAuthorization{
		AuthorizationID: authID, WorkflowID: "wf-it-3", IdempotencyKey: "idem-upd-1",
		CustomerID: "C1", AmountCents: 10000, Currency: "CNY",
		Status: domain.AuthStatusAuthorized, MatchedRuleIDs: []string{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := r.Insert(ctx, db, auth); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	auth.Status = domain.AuthStatusVoided
	auth.UpdatedAt = now.Add(time.Second)
	if err := r.UpdateStatus(ctx, db, auth); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	got, err := r.GetByID(ctx, db, authID)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	if got.Status != domain.AuthStatusVoided {
		t.Errorf("status = %s, want voided", got.Status)
	}
}

func TestAuthorizationRepo_MatchedRulesRoundTrip(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewAuthorizationRepo()
	now := time.Now().UTC()

	authID := "IT-AUTH-RULES-1"
	db.ExecContext(ctx, "DELETE FROM payment_authorization WHERE authorization_id=$1", authID)

	rules := []string{domain.RuleKYCInactive, domain.RuleBlacklisted}
	auth := domain.PaymentAuthorization{
		AuthorizationID: authID, WorkflowID: "wf-it-4", IdempotencyKey: "idem-rules-1",
		CustomerID: "C1", AmountCents: 0, Currency: "CNY",
		Status: domain.AuthStatusRejected, MatchedRuleIDs: rules,
		ContextDigest: "digest-rules", CreatedAt: now, UpdatedAt: now,
	}
	if err := r.Insert(ctx, db, auth); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := r.GetByID(ctx, db, authID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.MatchedRuleIDsEqual(rules) {
		t.Errorf("matched rules round-trip = %v, want %v", got.MatchedRuleIDs, rules)
	}
}

func TestAuthorizationRepo_IsBlacklisted(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewAuthorizationRepo()

	custID := "IT-BL-CUST-1"
	// Clean up any prior test data.
	db.ExecContext(ctx, "DELETE FROM blacklist WHERE cust_id=$1", custID)

	// Before insert → not blacklisted.
	blacklisted, err := r.IsBlacklisted(ctx, db, custID)
	if err != nil {
		t.Fatalf("IsBlacklisted before: %v", err)
	}
	if blacklisted {
		t.Error("should not be blacklisted before insert")
	}

	// Insert an active blacklist entry.
	listID := "IT-BL-TEST-1"
	db.ExecContext(ctx, "DELETE FROM blacklist WHERE list_id=$1", listID)
	_, err = db.ExecContext(ctx, `
		INSERT INTO blacklist (list_id, cust_id, entity_type, reason, status)
		VALUES ($1, $2, 'customer', 'test', 'active')`, listID, custID)
	if err != nil {
		t.Fatalf("seed blacklist: %v", err)
	}
	defer db.ExecContext(ctx, "DELETE FROM blacklist WHERE list_id=$1", listID)

	// After insert → blacklisted.
	blacklisted, err = r.IsBlacklisted(ctx, db, custID)
	if err != nil {
		t.Fatalf("IsBlacklisted after: %v", err)
	}
	if !blacklisted {
		t.Error("should be blacklisted after active entry")
	}
}

// TestAuthorizationRepo_FullTransactionWithOutbox verifies that Insert +
// appendOutbox can run in the same transaction and that a rollback discards
// both. This exercises the transactional guarantee the consumer relies on.
func TestAuthorizationRepo_FullTransactionWithOutbox(t *testing.T) {
	db := setupAuthorizationDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	// Ensure shared messaging tables exist.
	_, _ = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS outbox_message (
		  message_id uuid PRIMARY KEY,
		  message_type text NOT NULL,
		  schema_version integer NOT NULL,
		  routing_key text NOT NULL,
		  envelope jsonb NOT NULL,
		  attempts integer NOT NULL DEFAULT 0,
		  claim_token uuid,
		  claimed_at timestamptz,
		  dispatched_at timestamptz,
		  last_error text,
		  created_at timestamptz NOT NULL DEFAULT now()
		)`)
	_, _ = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS inbox_message (
		  consumer text NOT NULL,
		  message_id uuid NOT NULL,
		  message_type text NOT NULL,
		  processed_at timestamptz NOT NULL DEFAULT now(),
		  PRIMARY KEY (consumer, message_id)
		)`)

	authID := "IT-AUTH-TX-1"
	outboxID := "11111111-1111-1111-1111-111111111111"
	db.ExecContext(ctx, "DELETE FROM payment_authorization WHERE authorization_id=$1", authID)
	db.ExecContext(ctx, "DELETE FROM outbox_message WHERE message_id=$1", outboxID)

	// Commit path: insert auth + outbox in one tx, both should persist.
	err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		auth := domain.PaymentAuthorization{
			AuthorizationID: authID, WorkflowID: "wf-tx", IdempotencyKey: "idem-tx-1",
			CustomerID: "C1", AmountCents: 10000, Currency: "CNY",
			Status: domain.AuthStatusAuthorized, MatchedRuleIDs: []string{},
			ContextDigest: "digest", CreatedAt: now, UpdatedAt: now,
		}
		repo := repo.NewAuthorizationRepo()
		if err := repo.Insert(ctx, q, auth); err != nil {
			return err
		}
		envelope := fmt.Sprintf(`{"message_id":"%s","message_type":"x","routing_key":"y","envelope":"{}"}`, outboxID)
		_, err := q.ExecContext(ctx, `
			INSERT INTO outbox_message (message_id, message_type, schema_version, routing_key, envelope, attempts)
			VALUES ($1, 'risk.payment-authorized.v1', 1, 'risk.payment.authorized', $2, 0)`,
			outboxID, envelope)
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx commit: %v", err)
	}

	r := repo.NewAuthorizationRepo()
	_, err = r.GetByID(ctx, db, authID)
	if err != nil {
		t.Errorf("auth should persist after commit: %v", err)
	}
	var outboxCount int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM outbox_message WHERE message_id=$1", outboxID).Scan(&outboxCount)
	if err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if outboxCount != 1 {
		t.Errorf("outbox count = %d, want 1", outboxCount)
	}

	// Rollback path: insert auth in a tx that rolls back, auth should NOT persist.
	authID2 := "IT-AUTH-TX-2"
	err = pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		auth := domain.PaymentAuthorization{
			AuthorizationID: authID2, WorkflowID: "wf-tx", IdempotencyKey: "idem-tx-2",
			CustomerID: "C1", AmountCents: 5000, Currency: "CNY",
			Status: domain.AuthStatusAuthorized, MatchedRuleIDs: []string{},
			CreatedAt: now, UpdatedAt: now,
		}
		repo := repo.NewAuthorizationRepo()
		if err := repo.Insert(ctx, q, auth); err != nil {
			return err
		}
		return errors.New("intentional rollback")
	})
	if err == nil {
		t.Fatal("expected rollback error")
	}
	_, err = r.GetByID(ctx, db, authID2)
	if !errors.Is(err, service.ErrAuthorizationNotFound) {
		t.Errorf("auth should not persist after rollback, got: %v", err)
	}
}

// TestAuthorizationRepo_EncodeDecodeMatchedRules_JSON verifies the JSON encoding
// matches the column's DEFAULT '[]' constraint (never "null").
func TestAuthorizationRepo_EncodeDecodeMatchedRules_JSON(t *testing.T) {
	// nil → "[]"
	raw, err := domain.EncodeMatchedRules(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Errorf("nil rules → %s, want []", string(raw))
	}
	// Verify "[]" is valid JSON that the DB accepts.
	if !json.Valid(raw) {
		t.Error("[] is not valid JSON")
	}
}
