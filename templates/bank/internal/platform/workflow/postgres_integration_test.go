//go:build integration

package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/migrate"
	"bank/internal/platform/pg"
)

// ---------------------------------------------------------------------------
// Task 7 integration tests for PostgresStore.
//
// These tests require a live, migrated pay_db at the connection target
// pg.Open("pay_db") resolves to. They follow the existing integration-test
// convention (see internal/corebanking/repo/integration_test.go): on Open/Ping
// failure the test calls t.Skipf; otherwise the test runs against a real
// Postgres. The test process does NOT manage the container — operators are
// expected to keep one running on :15432 (or via env overrides).
//
// Each test ensures the rows it touches are deleted first so re-runs are
// deterministic even if a previous run left residue.
// ---------------------------------------------------------------------------

// setupPgStore opens pay_db, applies migrations idempotently, and returns a
// PostgresStore bound to the pool plus the underlying *sql.DB (so tests can run
// ad-hoc cleanup queries). Tests t.Skip on Open/Ping failure.
func setupPgStore(t *testing.T) (*PostgresStore, *sql.DB) {
	t.Helper()
	db, err := pg.Open("pay_db")
	if err != nil {
		t.Skipf("pg.Open(pay_db) failed; skipping: %v", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		t.Skipf("postgres not ready on pay_db; skipping (start one on :15432): %v", err)
	}
	// Apply migrations in the canonical order: shared.sql (outbox + inbox)
	// then pay_db.sql (transfer_txn, workflow_instance, workflow_action, ...).
	// migrate.Run is idempotent because every statement uses IF NOT EXISTS.
	// CWD for the test runner is internal/platform/workflow/ — three levels
	// up reaches templates/bank/.
	ctx := context.Background()
	for _, name := range []string{"shared.sql", "pay_db.sql"} {
		ddl, err := os.ReadFile(filepath.Join("..", "..", "..", "db", "migrations", name))
		if err != nil {
			db.Close()
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := migrate.Run(ctx, db, string(ddl)); err != nil {
			db.Close()
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return NewPostgresStore(db), db
}

// cleanupWorkflow removes all rows associated with workflowID so each test
// starts from a clean slate (outbox/inbox may otherwise accumulate across
// re-runs and break count assertions).
func cleanupWorkflow(t *testing.T, db *sql.DB, workflowID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`DELETE FROM outbox_message WHERE envelope->>'workflow_id' = $1`, workflowID); err != nil {
		// envelope->>'workflow_id' indexes the JSON field; if the column
		// lacks that key the row just won't match, which is fine.
		t.Logf("cleanup outbox for %s: %v (continuing)", workflowID, err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM workflow_action WHERE workflow_id = $1`, workflowID); err != nil {
		t.Fatalf("cleanup workflow_action for %s: %v", workflowID, err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM workflow_instance WHERE workflow_id = $1`, workflowID); err != nil {
		t.Fatalf("cleanup workflow_instance for %s: %v", workflowID, err)
	}
}

// outboxCountFor returns the number of outbox_message rows whose envelope is
// addressed to workflowID (envelope->>'workflow_id').
func outboxCountFor(t *testing.T, db *sql.DB, workflowID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM outbox_message WHERE envelope->>'workflow_id' = $1`,
		workflowID).Scan(&n); err != nil {
		t.Fatalf("count outbox for %s: %v", workflowID, err)
	}
	return n
}

// inboxCountFor returns the count of inbox_message rows for consumer+messageID.
func inboxCountFor(t *testing.T, db *sql.DB, consumer, messageID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM inbox_message WHERE consumer = $1 AND message_id = $2`,
		consumer, messageID).Scan(&n); err != nil {
		t.Fatalf("count inbox for %s/%s: %v", consumer, messageID, err)
	}
	return n
}

// loadInstanceDirect loads an Instance via a fresh read-only query (no lock)
// for test assertions; bypasses the Store so we observe committed state only.
func loadInstanceDirect(t *testing.T, db *sql.DB, workflowID string) Instance {
	t.Helper()
	row := db.QueryRowContext(context.Background(), `
		SELECT workflow_id, type, definition_version, status, input_json,
		       prepared_context_json, current_action, revision, lease_owner,
		       lease_until, next_wakeup_at, operational_deadline,
		       last_error_class, last_error
		FROM workflow_instance WHERE workflow_id = $1`, workflowID)
	inst, err := scanInstanceRow(row)
	if err != nil {
		t.Fatalf("loadInstanceDirect %s: %v", workflowID, err)
	}
	rows, err := db.QueryContext(context.Background(), `
		SELECT action_index, name, status, direction, attempt, idempotency_key,
		       command_id, result_event_id, deadline_at, output,
		       last_error_class, last_error, accepted_result_types
		FROM workflow_action WHERE workflow_id = $1 ORDER BY action_index`, workflowID)
	if err != nil {
		t.Fatalf("load actions %s: %v", workflowID, err)
	}
	defer rows.Close()
	for rows.Next() {
		rec, err := scanActionRow(rows)
		if err != nil {
			t.Fatalf("scan action %s: %v", workflowID, err)
		}
		inst.Actions = append(inst.Actions, rec)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iter actions %s: %v", workflowID, err)
	}
	return inst
}

// rowScanner is the *sql.Row / *sql.Rows common Scan surface.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanInstanceRow(s rowScanner) (Instance, error) {
	var (
		inst              Instance
		preparedCtx       []byte
		input             []byte
		leaseOwner        sql.NullString
		leaseUntil        sql.NullTime
		nextWakeup        sql.NullTime
		operational       sql.NullTime
		lastErrorClass    sql.NullString
		lastError         sql.NullString
	)
	if err := s.Scan(
		&inst.ID, &inst.Type, &inst.Version, &inst.Status, &input, &preparedCtx,
		&inst.CurrentAction, &inst.Revision, &leaseOwner, &leaseUntil, &nextWakeup,
		&operational, &lastErrorClass, &lastError,
	); err != nil {
		return Instance{}, err
	}
	inst.Input = json.RawMessage(input)
	if len(preparedCtx) > 0 {
		inst.PreparedContext = append(json.RawMessage(nil), preparedCtx...)
	}
	if leaseOwner.Valid {
		inst.LeaseOwner = leaseOwner.String
	}
	if leaseUntil.Valid {
		inst.LeaseUntil = leaseUntil.Time
	}
	if nextWakeup.Valid {
		inst.NextWakeupAt = nextWakeup.Time
	}
	if operational.Valid {
		inst.OperationalDeadline = operational.Time
	}
	if lastErrorClass.Valid {
		inst.LastErrorClass = ErrorClass(lastErrorClass.String)
	}
	if lastError.Valid {
		inst.LastError = lastError.String
	}
	return inst, nil
}

func scanActionRow(s rowScanner) (ActionRecord, error) {
	var (
		rec           ActionRecord
		idemKey       string
		status        string
		direction     string
		commandID     sql.NullString
		resultID      sql.NullString
		deadlineAt    sql.NullTime
		output        []byte
		errClass      sql.NullString
		errMsg        sql.NullString
		acceptedTypes []byte
	)
	if err := s.Scan(
		&rec.Index, &rec.Name, &status, &direction, &rec.Attempt, &idemKey,
		&commandID, &resultID, &deadlineAt, &output, &errClass, &errMsg,
		&acceptedTypes,
	); err != nil {
		return ActionRecord{}, err
	}
	rec.Status = ActionStatus(status)
	rec.Direction = direction
	rec.IdempotencyKey = idemKey
	if commandID.Valid {
		rec.CommandID = commandID.String
	}
	if resultID.Valid {
		rec.ResultEventID = resultID.String
	}
	if deadlineAt.Valid {
		rec.DeadlineAt = deadlineAt.Time
	}
	if len(output) > 0 {
		rec.Output = append(json.RawMessage(nil), output...)
	}
	if errClass.Valid {
		rec.LastErrorClass = ErrorClass(errClass.String)
	}
	if errMsg.Valid {
		rec.LastError = errMsg.String
	}
	if len(acceptedTypes) > 0 {
		if err := json.Unmarshal(acceptedTypes, &rec.AcceptedResultTypes); err != nil {
			return ActionRecord{}, fmt.Errorf("decode accepted_result_types: %w", err)
		}
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Scenario (a): two concurrent result transactions produce ONE revision
// increment. The Inbox dedup + row lock serialize duplicate deliveries so the
// handler runs once and Revision advances by exactly one (not two).
// ---------------------------------------------------------------------------

func TestPostgres_ConcurrentDuplicateResult_OneRevisionIncrement(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const wfID = "wf-pg-concurrent-result"
	cleanupWorkflow(t, db, wfID)

	// 2-action workflow; first action succeeds so a successful result will
	// both record output AND dispatch the next action — but we only deliver
	// once because the duplicate is deduped.
	def := linearDef{
		workflowType: "payment-transfer",
		version:      1,
		actions: []Action{
			linearAction{name: "book-transfer", routingKey: "bookings.cmd"},
			linearAction{name: "settle-transfer", routingKey: "settlements.cmd"},
		},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})

	ctx := context.Background()
	if _, err := engine.Start(ctx, StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	// Capture the in-flight command ID for the current action.
	inst := loadInstanceDirect(t, db, wfID)
	if len(inst.Actions) == 0 {
		t.Fatalf("no actions after Prepare")
	}
	cmdID := inst.Actions[0].CommandID
	if cmdID == "" {
		t.Fatal("CommandID empty after Prepare")
	}

	// Build ONE result envelope; both goroutines deliver the SAME envelope
	// (same MessageID) — the second delivery must be deduped by the Inbox.
	resultEnv := messaging.NewEnvelope(
		"result.book-transfer", wfID, json.RawMessage(`{"ok":true}`), time.Now,
	)
	resultEnv.WorkflowID = wfID
	resultEnv.ActionName = "book-transfer"
	resultEnv.CommandID = cmdID

	revBefore := loadInstanceDirect(t, db, wfID).Revision

	// Launch two concurrent deliveries on the same envelope.
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = engine.ApplyResult(ctx, resultEnv)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d ApplyResult: %v", i, err)
		}
	}

	// Exactly one Inbox row for (consumer, messageID).
	if got := inboxCountFor(t, db, resultConsumer, resultEnv.MessageID); got != 1 {
		t.Errorf("inbox rows = %d, want 1 (duplicate was not deduped)", got)
	}
	// Revision advanced exactly once despite two concurrent deliveries.
	got := loadInstanceDirect(t, db, wfID)
	if got.Revision != revBefore+1 {
		t.Errorf("Revision = %d, want %d (exactly ONE increment)",
			got.Revision, revBefore+1)
	}
	// The current action advanced to index 1 (settle-transfer) — proving the
	// handler ran exactly once and the duplicate was a true no-op.
	if got.CurrentAction != 1 {
		t.Errorf("CurrentAction = %d, want 1 (single advance)", got.CurrentAction)
	}
}

// ---------------------------------------------------------------------------
// Scenario (b): Outbox rollback when Action save fails. A WithInstance
// callback that AppendOutbox-es successfully then SaveAction-s with an invalid
// status must roll back BOTH writes — no new outbox row, no revision bump.
// ---------------------------------------------------------------------------

func TestPostgres_OutboxRollsBackOnActionSaveFailure(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const wfID = "wf-pg-outbox-rollback"
	cleanupWorkflow(t, db, wfID)

	def := linearDef{
		workflowType: "payment-transfer", version: 1,
		actions: []Action{linearAction{name: "book-transfer", routingKey: "bookings.cmd"}},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	ctx := context.Background()
	if _, err := engine.Start(ctx, StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	outboxBefore := outboxCountFor(t, db, wfID)
	if outboxBefore != 1 {
		t.Fatalf("outbox count after Prepare = %d, want 1 (the dispatch)", outboxBefore)
	}
	revBefore := loadInstanceDirect(t, db, wfID).Revision

	// Inside one WithInstance callback: SaveInstance, AppendOutbox, then a
	// SaveAction with a status value that violates the CHECK constraint
	// (ck_workflow_action_status). The whole tx must roll back.
	err := store.WithInstance(ctx, wfID, func(tx Tx) error {
		current := tx.Instance()
		bumped := *current
		bumped.Revision++ // pretend we're advancing
		if err := tx.SaveInstance(bumped); err != nil {
			return fmt.Errorf("save instance: %w", err)
		}
		env := messaging.NewEnvelope(
			"command.synthetic", wfID, json.RawMessage(`{}`), time.Now,
		)
		env.WorkflowID = wfID
		if err := tx.AppendOutbox(env, "synthetic.cmd"); err != nil {
			return fmt.Errorf("append outbox: %w", err)
		}
		// Save an action with an INVALID status — violates CHECK constraint,
		// forcing the surrounding transaction to roll back.
		return tx.SaveAction(ActionRecord{
			Index: 99, Name: "synthetic", Status: ActionStatus("bogus-status"),
			Direction: directionForward, IdempotencyKey: "synthetic-key",
		})
	})
	if err == nil {
		t.Fatal("WithInstance returned nil; want CHECK constraint violation")
	}

	// Outbox must NOT have the new row — rollback discarded it.
	if got := outboxCountFor(t, db, wfID); got != outboxBefore {
		t.Errorf("outbox count = %d, want %d (rollback did not discard outbox append)",
			got, outboxBefore)
	}
	// Revision must be unchanged — SaveInstance was rolled back too.
	if rev := loadInstanceDirect(t, db, wfID).Revision; rev != revBefore {
		t.Errorf("Revision = %d, want %d (SaveInstance was not rolled back)",
			rev, revBefore)
	}
}

// ---------------------------------------------------------------------------
// Scenario (c): expired lease is reclaimed via FOR UPDATE SKIP LOCKED. The
// claim SQL must SKIP rows still locked by another tx AND skip rows whose
// lease has not yet expired, while picking up rows whose lease IS expired.
// ---------------------------------------------------------------------------

func TestPostgres_ClaimRunnable_SkipLockedExpiredLease(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const (
		wfExpired  = "wf-pg-claim-expired"
		wfHeld     = "wf-pg-claim-held"
		wfFresh    = "wf-pg-claim-fresh"
	)
	for _, id := range []string{wfExpired, wfHeld, wfFresh} {
		cleanupWorkflow(t, db, id)
	}

	def := linearDef{
		workflowType: "payment-transfer", version: 1,
		actions: []Action{linearAction{name: "book-transfer", routingKey: "bookings.cmd"}},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	ctx := context.Background()
	for _, id := range []string{wfExpired, wfHeld, wfFresh} {
		if _, err := engine.Start(ctx, StartRequest{
			WorkflowID: id, Type: "payment-transfer", Version: 1,
			Input: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		// Preparing instances are runnable per isRunnable.
	}

	now := time.Now()
	// wfExpired: lease held by owner-A but PAST expiry — must be claimable.
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_instance SET lease_owner=$1, lease_until=$2, revision=revision+1
		WHERE workflow_id=$3`,
		"owner-A", now.Add(-1*time.Minute), wfExpired); err != nil {
		t.Fatal(err)
	}
	// wfHeld: lease held by owner-A and STILL ACTIVE — must be skipped.
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_instance SET lease_owner=$1, lease_until=$2, revision=revision+1
		WHERE workflow_id=$3`,
		"owner-A", now.Add(+5*time.Minute), wfHeld); err != nil {
		t.Fatal(err)
	}
	// wfFresh: no lease — must be claimable.

	// owner-B claims: should get wfExpired + wfFresh, NOT wfHeld.
	ids, err := store.ClaimRunnable(ctx, "owner-B", now, 30*time.Second, 10)
	if err != nil {
		t.Fatalf("ClaimRunnable: %v", err)
	}
	if !containsString(ids, wfExpired) {
		t.Errorf("ClaimRunnable ids=%v; want to include %q (expired lease should be reclaimable)", ids, wfExpired)
	}
	if !containsString(ids, wfFresh) {
		t.Errorf("ClaimRunnable ids=%v; want to include %q (no lease should be claimable)", ids, wfFresh)
	}
	if containsString(ids, wfHeld) {
		t.Errorf("ClaimRunnable ids=%v; must NOT include %q (lease still active)", ids, wfHeld)
	}

	// Verify the rows were actually mutated: lease owner is now owner-B with
	// a future lease_until on the expired-lease row.
	expired := loadInstanceDirect(t, db, wfExpired)
	if expired.LeaseOwner != "owner-B" {
		t.Errorf("wfExpired.LeaseOwner = %q, want owner-B (stolen after expiry)",
			expired.LeaseOwner)
	}
	if !expired.LeaseUntil.After(now) {
		t.Errorf("wfExpired.LeaseUntil = %v, want a future time (now+lease)",
			expired.LeaseUntil)
	}
	// wfHeld's lease must remain untouched.
	held := loadInstanceDirect(t, db, wfHeld)
	if held.LeaseOwner != "owner-A" {
		t.Errorf("wfHeld.LeaseOwner = %q, want owner-A (lease must not be stolen)",
			held.LeaseOwner)
	}
}

// ---------------------------------------------------------------------------
// Scenario (d): duplicate Inbox event does not invoke the handler. Two
// ApplyResult calls with the same MessageID on a fresh instance — the action's
// ApplyResult runs exactly once.
// ---------------------------------------------------------------------------

func TestPostgres_DuplicateInboxEvent_DoesNotInvokeHandler(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const wfID = "wf-pg-inbox-dedup"
	cleanupWorkflow(t, db, wfID)

	// countingAction records each ApplyResult invocation; the test asserts
	// exactly one call across two duplicate deliveries.
	counter := &countingAction{
		name:    "book-transfer",
		inner:   linearAction{name: "book-transfer", routingKey: "bookings.cmd"},
		outcome: Outcome{Succeeded: true, Output: json.RawMessage(`{"ok":true}`)},
	}
	def := linearDef{
		workflowType: "payment-transfer", version: 1,
		actions: []Action{counter},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	ctx := context.Background()
	if _, err := engine.Start(ctx, StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}
	cmdID := loadInstanceDirect(t, db, wfID).Actions[0].CommandID

	// Build ONE result envelope (single MessageID). Deliver it twice.
	resultEnv := messaging.NewEnvelope(
		"result.book-transfer", wfID, json.RawMessage(`{"ok":true}`), time.Now,
	)
	resultEnv.WorkflowID = wfID
	resultEnv.ActionName = "book-transfer"
	resultEnv.CommandID = cmdID

	if err := engine.ApplyResult(ctx, resultEnv); err != nil {
		t.Fatalf("first ApplyResult: %v", err)
	}
	// Reset counter so we only count the second (duplicate) delivery's handler
	// invocations. The first delivery should have invoked the handler once.
	firstCalls := counter.calls()
	if firstCalls != 1 {
		t.Fatalf("first delivery invoked handler %d times, want 1", firstCalls)
	}

	// Second delivery: SAME MessageID — must be deduped, handler not invoked.
	if err := engine.ApplyResult(ctx, resultEnv); err != nil {
		t.Fatalf("second (duplicate) ApplyResult: %v", err)
	}
	if got := counter.calls(); got != 1 {
		t.Errorf("handler invoked %d times total after duplicate delivery, want 1 (dedup)",
			got)
	}
	// Instance moved to succeeded after the first delivery; second was a no-op.
	got := loadInstanceDirect(t, db, wfID)
	if got.Status != StatusSucceeded {
		t.Errorf("Status = %q, want succeeded (first delivery advanced, second no-op)",
			got.Status)
	}
}

// countingAction wraps an inner Action and counts ApplyResult invocations via
// an atomic counter. All other methods delegate to the inner action.
type countingAction struct {
	name    string
	inner   Action
	outcome Outcome
	n       atomic.Int64
}

func (a *countingAction) Name() string { return a.name }
func (a *countingAction) Execute(ctx context.Context, v View) (Dispatch, error) {
	return a.inner.Execute(ctx, v)
}
func (a *countingAction) ApplyResult(ctx context.Context, v View, env messaging.Envelope) (Outcome, error) {
	a.n.Add(1)
	return a.outcome, nil
}
func (a *countingAction) Compensate(ctx context.Context, v View) (Dispatch, error) {
	return a.inner.Compensate(ctx, v)
}
func (a *countingAction) ApplyCompensationResult(ctx context.Context, v View, env messaging.Envelope) (Outcome, error) {
	return a.inner.ApplyCompensationResult(ctx, v, env)
}
func (a *countingAction) calls() int64 { return a.n.Load() }

// ---------------------------------------------------------------------------
// Sanity: Create rejects duplicates with ErrInstanceExists.
// ---------------------------------------------------------------------------

func TestPostgres_CreateRejectsDuplicate(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const wfID = "wf-pg-create-dup"
	cleanupWorkflow(t, db, wfID)

	ctx := context.Background()
	if _, err := store.Create(ctx, StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Create(ctx, StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrInstanceExists) {
		t.Errorf("duplicate Create: error=%v, want ErrInstanceExists", err)
	}
}

// ---------------------------------------------------------------------------
// Sanity: RenewLease returns ErrLeaseNotHeld when caller does not own the
// lease, ErrInstanceNotFound when the instance is missing.
// ---------------------------------------------------------------------------

func TestPostgres_RenewReleaseLeaseSemantics(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const wfID = "wf-pg-lease"
	cleanupWorkflow(t, db, wfID)

	ctx := context.Background()
	if _, err := store.Create(ctx, StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	// Initially no lease owner — RenewLease by owner-A fails with ErrLeaseNotHeld.
	if err := store.RenewLease(ctx, wfID, "owner-A", now, 30*time.Second); !errors.Is(err, ErrLeaseNotHeld) {
		t.Errorf("RenewLease with no prior lease: error=%v, want ErrLeaseNotHeld", err)
	}
	// Claim to grant a lease to owner-A.
	ids, err := store.ClaimRunnable(ctx, "owner-A", now, 30*time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(ids, wfID) {
		t.Fatalf("ClaimRunnable did not return %q (preparing must be runnable); got %v",
			wfID, ids)
	}
	// RenewLease by owner-A succeeds; by owner-B fails with ErrLeaseNotHeld.
	if err := store.RenewLease(ctx, wfID, "owner-A", now, 60*time.Second); err != nil {
		t.Errorf("RenewLease by owner-A: %v", err)
	}
	if err := store.RenewLease(ctx, wfID, "owner-B", now, 60*time.Second); !errors.Is(err, ErrLeaseNotHeld) {
		t.Errorf("RenewLease by owner-B: error=%v, want ErrLeaseNotHeld", err)
	}
	// ReleaseLease by owner-C is a no-op (lease held by owner-A).
	if err := store.ReleaseLease(ctx, wfID, "owner-C"); err != nil {
		t.Errorf("ReleaseLease by non-owner: %v (must be no-op)", err)
	}
	// ReleaseLease by owner-A succeeds; instance's lease_owner is cleared.
	if err := store.ReleaseLease(ctx, wfID, "owner-A"); err != nil {
		t.Errorf("ReleaseLease by owner-A: %v", err)
	}
	got := loadInstanceDirect(t, db, wfID)
	if got.LeaseOwner != "" {
		t.Errorf("LeaseOwner = %q, want empty (released)", got.LeaseOwner)
	}

	// Missing instance: RenewLease → ErrInstanceNotFound; ReleaseLease → no-op.
	if err := store.RenewLease(ctx, "wf-missing", "owner-A", now, 30*time.Second); !errors.Is(err, ErrInstanceNotFound) {
		t.Errorf("RenewLease on missing: error=%v, want ErrInstanceNotFound", err)
	}
	if err := store.ReleaseLease(ctx, "wf-missing", "owner-A"); err != nil {
		t.Errorf("ReleaseLease on missing: %v (must be silent no-op)", err)
	}
}

// ---------------------------------------------------------------------------
// Sanity: TimedOut and NonTerminalDefinitions return correct instances.
// ---------------------------------------------------------------------------

func TestPostgres_TimedOutAndNonTerminalDefinitions(t *testing.T) {
	store, db := setupPgStore(t)
	defer db.Close()

	const wfTimedOut = "wf-pg-timed-out"
	const wfTerminal = "wf-pg-terminal"
	for _, id := range []string{wfTimedOut, wfTerminal} {
		cleanupWorkflow(t, db, id)
	}

	def := linearDef{
		workflowType: "payment-transfer", version: 1,
		actions: []Action{linearAction{name: "book-transfer", routingKey: "bookings.cmd"}},
	}
	engine := NewEngine(store, registryWith(def), EngineConfig{})
	ctx := context.Background()

	// wfTimedOut: drive to running with a waiting action, then backdate its deadline.
	if _, err := engine.Start(ctx, StartRequest{
		WorkflowID: wfTimedOut, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfTimedOut); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-1 * time.Minute)
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_action SET deadline_at=$1 WHERE workflow_id=$2 AND action_index=0`,
		past, wfTimedOut); err != nil {
		t.Fatal(err)
	}

	// wfTerminal: mark succeeded directly so it's excluded from both queries.
	if _, err := store.Create(ctx, StartRequest{
		WorkflowID: wfTerminal, Type: "payment-transfer", Version: 1,
		Input: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE workflow_instance SET status='succeeded' WHERE workflow_id=$1`, wfTerminal); err != nil {
		t.Fatal(err)
	}

	// TimedOut surfaces wfTimedOut, NOT wfTerminal.
	now := time.Now()
	timed, err := store.TimedOut(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(timed, wfTimedOut) {
		t.Errorf("TimedOut = %v; want to include %q", timed, wfTimedOut)
	}
	if containsString(timed, wfTerminal) {
		t.Errorf("TimedOut = %v; must NOT include terminal %q", timed, wfTerminal)
	}

	// NonTerminalDefinitions surfaces (payment-transfer,1) once from wfTimedOut;
	// wfTerminal is terminal and excluded.
	refs, err := store.NonTerminalDefinitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := DefinitionRef{Type: "payment-transfer", Version: 1}
	count := 0
	for _, r := range refs {
		if r == want {
			count++
		}
	}
	if count == 0 {
		t.Errorf("NonTerminalDefinitions = %v; want at least one %v", refs, want)
	}
}
