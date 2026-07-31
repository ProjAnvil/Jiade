//go:build integration

// Package saga_test exercises the full payment-transfer saga against real
// Postgres databases (pay_db, risk_db, core_db) and the real workflow engine.
// No broker is used: dispatched commands are read from the pay_db outbox and
// fed to the real risk/core domain services directly; the service result is
// converted to a result envelope and handed to Engine.ApplyResult. This tests
// the real PostgresStore + real engine + real PaymentTransferDefinition + real
// domain services end-to-end.
//
// Build tag: //go:build integration
// Run: go test -tags=integration -p 1 ./...
//
// Convention: each test calls t.Skipf when PG is unavailable (see
// internal/corebanking/repo/integration_test.go). The tests clean up their own
// rows so re-runs are deterministic.
package saga_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bank/internal/corebanking/domain"
	corerepo "bank/internal/corebanking/repo"
	coreservice "bank/internal/corebanking/service"
	"bank/internal/platform/messaging"
	"bank/internal/platform/migrate"
	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
	"bank/internal/platform/workflow"
	"bank/internal/payment/workflows"
	riskdomain "bank/internal/risk/domain"
	riskrepo "bank/internal/risk/repo"
	riskservice "bank/internal/risk/service"
)

// ---------------------------------------------------------------------------
// sagaEnv — shared connections and wired services for one test run.
// ---------------------------------------------------------------------------

type sagaEnv struct {
	payDB      *sql.DB
	riskDB     *sql.DB
	coreDB     *sql.DB
	store      *workflow.PostgresStore
	engine     *workflow.Engine
	riskSvc    *riskservice.AuthorizationService
	holdSvc    *coreservice.HoldService
	transferSvc *coreservice.HeldTransferService
	customers  stubCustomerReader
	// resultIDs collects every result envelope MessageID fed to ApplyResult,
	// so the inbox count can verify dedup regardless of action-record
	// result_event_id overwrites during compensation.
	resultIDs  []string
}

// stubCustomerReader satisfies serviceclient.CustomerReader for both the
// Preparation and the risk service without a gRPC round-trip.
type stubCustomerReader map[string]serviceclient.Customer

func (s stubCustomerReader) GetCustomer(_ context.Context, id, _ string) (serviceclient.Customer, error) {
	if c, ok := s[id]; ok {
		return c, nil
	}
	return serviceclient.Customer{}, fmt.Errorf("stub: customer %q not found", id)
}

// stubAccountReader satisfies serviceclient.AccountReader for the Preparation.
type stubAccountReader map[string]serviceclient.Account

func (s stubAccountReader) GetAccount(_ context.Context, no, _ string) (serviceclient.Account, error) {
	if a, ok := s[no]; ok {
		return a, nil
	}
	return serviceclient.Account{}, fmt.Errorf("stub: account %q not found", no)
}

// setupSagaEnv opens all three databases, applies migrations, and wires the
// real domain services + engine. The caller defers cleanup.
func setupSagaEnv(t *testing.T) *sagaEnv {
	t.Helper()
	payDB, err := pg.Open("pay_db")
	if err != nil {
		t.Skipf("pg.Open(pay_db) failed; skipping: %v", err)
	}
	if err := payDB.PingContext(context.Background()); err != nil {
		payDB.Close()
		t.Skipf("postgres not ready on pay_db; skipping (start one on :15432): %v", err)
	}
	riskDB, err := pg.Open("risk_db")
	if err != nil {
		payDB.Close()
		t.Skipf("pg.Open(risk_db) failed; skipping: %v", err)
	}
	coreDB, err := pg.Open("core_db")
	if err != nil {
		payDB.Close()
		riskDB.Close()
		t.Skipf("pg.Open(core_db) failed; skipping: %v", err)
	}

	env := &sagaEnv{payDB: payDB, riskDB: riskDB, coreDB: coreDB}

	// Apply migrations to pay_db only (the bank user owns pay_db and can
	// CREATE TABLE). risk_db and core_db are owned by bossy; their schemas
	// are pre-provisioned (see internal/corebanking/repo/integration_test.go
	// for the same convention). The bank user can still INSERT/UPDATE/DELETE.
	applyMigration(t, payDB, "shared.sql", "pay_db.sql")

	// Wire risk service (stateless repo; CustomerReader stub injected per scenario).
	env.customers = stubCustomerReader{}
	env.riskSvc = riskservice.NewAuthorizationService(riskrepo.NewAuthorizationRepo(), env.customers, time.Now)

	// Wire core-banking services.
	coreHR := corerepo.NewHoldRepo(coreDB)
	coreLR := corerepo.NewLedgerRepo(coreDB)
	coreAR := corerepo.NewAccountRepo(coreDB)
	coreLS := coreservice.NewLedgerService(coreLR)
	env.holdSvc = coreservice.NewHoldService(coreDB, coreHR)
	env.transferSvc = coreservice.NewHeldTransferService(coreDB, coreHR, coreAR, coreLS, coreLR, coreLR)

	// Wire payment workflow engine with the real PaymentTransferDefinition.
	// The Preparation uses stub readers (no gRPC); each scenario customises
	// the customer/account maps before calling startSaga.
	prepAccountReader := stubAccountReader{}
	preparation := workflows.NewPreparation(env.customers, prepAccountReader)
	env.store = workflow.NewPostgresStore(payDB)
	registry := workflow.NewRegistry()
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		t.Fatalf("register payment-transfer def: %v", err)
	}
	env.engine = workflow.NewEngine(env.store, registry, workflow.EngineConfig{})

	// Ensure biz_date is set in core_db for PostHeldTransfer.
	setBizDate(t, coreDB, "2026-07-16")

	return env
}

// applyMigration reads and applies DDL files from db/migrations.
func applyMigration(t *testing.T, db *sql.DB, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		ddl, err := os.ReadFile(filepath.Join("..", "db", "migrations", name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		if err := migrate.Run(ctx, db, string(ddl)); err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
}

// setBizDate sets sys_param.biz_date in core_db (required by PostHeldTransfer).
func setBizDate(t *testing.T, db *sql.DB, date string) {
	t.Helper()
	ctx := context.Background()
	db.ExecContext(ctx, `INSERT INTO sys_param (param_key, param_value) VALUES ('biz_date', $1)
		ON CONFLICT (param_key) DO UPDATE SET param_value = $1`, date)
}

// close closes all DB pools.
func (e *sagaEnv) close() {
	e.payDB.Close()
	e.riskDB.Close()
	e.coreDB.Close()
}

// ---------------------------------------------------------------------------
// Seed + cleanup helpers.
// ---------------------------------------------------------------------------

// seedAccount creates a demand_account + historical balance row in core_db.
func seedAccount(t *testing.T, db *sql.DB, acct, custID string, balanceYuan float64) {
	t.Helper()
	ctx := context.Background()
	db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := corerepo.NewAccountRepo(db).InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: custID, Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatalf("seed account %s: %v", acct, err)
	}
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',$2,$2,'2011')`, acct, balanceYuan)
}

// cleanupSaga removes all rows associated with wfID across all three DBs so
// each test starts from a clean slate.
func cleanupSaga(t *testing.T, env *sagaEnv, wfID string) {
	t.Helper()
	ctx := context.Background()
	// pay_db
	env.payDB.ExecContext(ctx, `DELETE FROM inbox_message WHERE message_id IN (
		SELECT (envelope->>'message_id')::uuid FROM outbox_message WHERE envelope->>'workflow_id' = $1)`, wfID)
	env.payDB.ExecContext(ctx, `DELETE FROM outbox_message WHERE envelope->>'workflow_id' = $1`, wfID)
	env.payDB.ExecContext(ctx, `DELETE FROM workflow_action WHERE workflow_id = $1`, wfID)
	env.payDB.ExecContext(ctx, `DELETE FROM workflow_instance WHERE workflow_id = $1`, wfID)
	// risk_db
	env.riskDB.ExecContext(ctx, `DELETE FROM payment_authorization WHERE workflow_id = $1`, wfID)
	env.riskDB.ExecContext(ctx, `DELETE FROM outbox_message WHERE envelope->>'workflow_id' = $1`, wfID)
	env.riskDB.ExecContext(ctx, `DELETE FROM inbox_message WHERE message_id IN (
		SELECT (envelope->>'message_id')::uuid FROM outbox_message WHERE envelope->>'workflow_id' = $1)`, wfID)
	// core_db
	env.coreDB.ExecContext(ctx, `DELETE FROM voucher_reversal WHERE reverses_voucher_no LIKE 'V%'`)
	env.coreDB.ExecContext(ctx, `DELETE FROM held_transfer WHERE idempotency_key LIKE $1`, "wf:"+wfID+"%")
	env.coreDB.ExecContext(ctx, `DELETE FROM funds_hold WHERE workflow_id = $1`, wfID)
	env.coreDB.ExecContext(ctx, `DELETE FROM outbox_message WHERE envelope->>'workflow_id' = $1`, wfID)
}

// cleanupAccounts removes test accounts and their entries from core_db.
func cleanupAccounts(t *testing.T, db *sql.DB, accounts ...string) {
	t.Helper()
	ctx := context.Background()
	for _, acct := range accounts {
		db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", acct)
		db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
		db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	}
}

// ---------------------------------------------------------------------------
// Row-count helpers.
// ---------------------------------------------------------------------------

func countRow(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed: %v\n  query: %s", err, query)
	}
	return n
}

func workflowStatus(t *testing.T, db *sql.DB, wfID string) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(context.Background(),
		`SELECT status FROM workflow_instance WHERE workflow_id=$1`, wfID).Scan(&status); err != nil {
		t.Fatalf("read workflow status for %s: %v", wfID, err)
	}
	return status
}

func assertCount(t *testing.T, label string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", label, got, want)
	}
}

// ---------------------------------------------------------------------------
// Saga driver — reads dispatched commands from the outbox and calls the real
// domain service to produce a result envelope.
// ---------------------------------------------------------------------------

// outboxCommand is a dispatched command read from pay_db outbox.
type outboxCommand struct {
	routingKey string
	env        messaging.Envelope
}

// readLastCommand reads the most recent undispatched outbox row for wfID.
func readLastCommand(t *testing.T, db *sql.DB, wfID string) outboxCommand {
	t.Helper()
	var routingKey string
	var envelopeJSON []byte
	err := db.QueryRowContext(context.Background(), `
		SELECT routing_key, envelope FROM outbox_message
		WHERE envelope->>'workflow_id' = $1 AND dispatched_at IS NULL
		ORDER BY created_at DESC LIMIT 1`, wfID,
	).Scan(&routingKey, &envelopeJSON)
	if err != nil {
		t.Fatalf("read outbox command for %s: %v", wfID, err)
	}
	var env messaging.Envelope
	if err := json.Unmarshal(envelopeJSON, &env); err != nil {
		t.Fatalf("decode outbox envelope: %v", err)
	}
	return outboxCommand{routingKey: routingKey, env: env}
}

// buildResult creates a result envelope carrying the correct workflow/action/
// command correlation from the command envelope.
func buildResult(cmd outboxCommand, messageType string, payload json.RawMessage) messaging.Envelope {
	env := messaging.NewEnvelope(messageType, cmd.env.CorrelationID, payload, time.Now)
	env.WorkflowID = cmd.env.WorkflowID
	env.ActionName = cmd.env.ActionName
	env.CommandID = cmd.env.CommandID
	return env
}

// driveAuthorize calls the risk AuthorizePayment service and returns the result
// envelope. The riskDB is used as the pg.DBTX.
func driveAuthorize(t *testing.T, env *sagaEnv, cmd outboxCommand) messaging.Envelope {
	t.Helper()
	ctx := context.Background()
	var p struct {
		AuthorizationID string `json:"authorization_id"`
		CustomerID      string `json:"customer_id"`
		AmountCents     int64  `json:"amount_cents"`
		Currency        string `json:"currency"`
	}
	if err := json.Unmarshal(cmd.env.Payload, &p); err != nil {
		t.Fatalf("decode authorize payload: %v", err)
	}
	result, err := env.riskSvc.AuthorizePayment(ctx, env.riskDB, riskservice.AuthorizeCommand{
		AuthorizationID: p.AuthorizationID,
		WorkflowID:      cmd.env.WorkflowID,
		IdempotencyKey:  cmd.env.IdempotencyKey,
		CustomerID:      p.CustomerID,
		AmountCents:     p.AmountCents,
		Currency:        p.Currency,
	})
	if err != nil {
		t.Fatalf("AuthorizePayment: %v", err)
	}
	var payload json.RawMessage
	if result.Authorization.Status == riskdomain.AuthStatusAuthorized {
		payload, _ = json.Marshal(map[string]string{"authorization_id": p.AuthorizationID})
	} else {
		payload, _ = json.Marshal(map[string]string{"authorization_id": p.AuthorizationID, "reason": "rejected"})
	}
	return buildResult(cmd, result.EventType, payload)
}

// driveVoid calls the risk VoidAuthorization service.
func driveVoid(t *testing.T, env *sagaEnv, cmd outboxCommand) messaging.Envelope {
	t.Helper()
	ctx := context.Background()
	var p struct {
		AuthorizationID string `json:"authorization_id"`
	}
	if err := json.Unmarshal(cmd.env.Payload, &p); err != nil {
		t.Fatalf("decode void payload: %v", err)
	}
	result, err := env.riskSvc.VoidAuthorization(ctx, env.riskDB, riskservice.VoidCommand{
		AuthorizationID: p.AuthorizationID,
		WorkflowID:      cmd.env.WorkflowID,
		IdempotencyKey:  cmd.env.IdempotencyKey,
	})
	if err != nil {
		t.Fatalf("VoidAuthorization: %v", err)
	}
	return buildResult(cmd, result.EventType, json.RawMessage(`{"authorization_id":"`+p.AuthorizationID+`"}`))
}

// driveHold calls the core PlaceHold service.
func driveHold(t *testing.T, env *sagaEnv, cmd outboxCommand) messaging.Envelope {
	t.Helper()
	ctx := context.Background()
	var p struct {
		AccountNo   string `json:"account_no"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
	}
	if err := json.Unmarshal(cmd.env.Payload, &p); err != nil {
		t.Fatalf("decode hold payload: %v", err)
	}
	hold, err := env.holdSvc.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: cmd.env.IdempotencyKey,
		AccountNo:      p.AccountNo,
		Amount:         domain.NewMoneyFromCents(p.AmountCents),
		Ccy:            p.Currency,
		WorkflowID:     cmd.env.WorkflowID,
	})
	if err != nil {
		// Build a hold-failed result with the appropriate error class.
		class := "transient_failure"
		msg := err.Error()
		if err == coreservice.ErrInsufficientAvailableBalance {
			class = "business_rejected"
		}
		failPayload, _ := json.Marshal(map[string]string{
			"error_class":   class,
			"error_message": msg,
			"workflow_id":   cmd.env.WorkflowID,
		})
		return buildResult(cmd, "core.hold-failed.v1", failPayload)
	}
	payload, _ := json.Marshal(map[string]string{"hold_id": hold.HoldID})
	return buildResult(cmd, "core.hold-placed.v1", payload)
}

// driveRelease calls the core ReleaseHold service.
func driveRelease(t *testing.T, env *sagaEnv, cmd outboxCommand) messaging.Envelope {
	t.Helper()
	ctx := context.Background()
	var p struct {
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal(cmd.env.Payload, &p); err != nil {
		t.Fatalf("decode release payload: %v", err)
	}
	hold, err := env.holdSvc.ReleaseHold(ctx, p.HoldID, cmd.env.IdempotencyKey)
	if err != nil {
		t.Fatalf("ReleaseHold: %v", err)
	}
	_ = hold
	return buildResult(cmd, "core.hold-released.v1", json.RawMessage(`{"hold_id":"`+p.HoldID+`"}`))
}

// driveTransfer calls the core PostHeldTransfer service.
func driveTransfer(t *testing.T, env *sagaEnv, cmd outboxCommand) messaging.Envelope {
	t.Helper()
	ctx := context.Background()
	var p struct {
		HoldID      string `json:"hold_id"`
		FromAccount string `json:"from_account"`
		ToAccount   string `json:"to_account"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
	}
	if err := json.Unmarshal(cmd.env.Payload, &p); err != nil {
		t.Fatalf("decode transfer payload: %v", err)
	}
	booking, err := env.transferSvc.PostHeldTransfer(ctx, coreservice.PostHeldTransfer{
		IdempotencyKey: cmd.env.IdempotencyKey,
		HoldID:         p.HoldID,
		FromAccount:    p.FromAccount,
		ToAccount:      p.ToAccount,
		Amount:         domain.NewMoneyFromCents(p.AmountCents),
		Ccy:            p.Currency,
	})
	if err != nil {
		t.Fatalf("PostHeldTransfer: %v", err)
	}
	payload, _ := json.Marshal(map[string]string{"voucher_no": booking.VoucherNo})
	return buildResult(cmd, coreservice.EventTransferPosted, payload)
}

// advance reads the last dispatched command, calls the corresponding service,
// feeds the result to the engine, and returns the result envelope.
func advance(t *testing.T, env *sagaEnv, wfID string) messaging.Envelope {
	t.Helper()
	cmd := readLastCommand(t, env.payDB, wfID)
	var result messaging.Envelope
	switch cmd.routingKey {
	case "risk.authorize-payment.v1":
		result = driveAuthorize(t, env, cmd)
	case "core.place-hold.v1":
		result = driveHold(t, env, cmd)
	case "core.post-held-transfer.v1":
		result = driveTransfer(t, env, cmd)
	case "risk.void-payment-authorization.v1":
		result = driveVoid(t, env, cmd)
	case "core.release-hold.v1":
		result = driveRelease(t, env, cmd)
	default:
		t.Fatalf("unknown routing key: %s", cmd.routingKey)
	}
	if err := env.engine.ApplyResult(context.Background(), result); err != nil {
		t.Fatalf("ApplyResult (%s): %v", cmd.routingKey, err)
	}
	env.resultIDs = append(env.resultIDs, result.MessageID)
	return result
}

// injectFailure builds a failure result envelope for the current command and
// feeds it to the engine, simulating a downstream service returning an error.
func injectFailure(t *testing.T, env *sagaEnv, wfID, resultType, errorClass, errorMessage string) {
	t.Helper()
	cmd := readLastCommand(t, env.payDB, wfID)
	payload, _ := json.Marshal(map[string]string{
		"error_class":   errorClass,
		"error_message": errorMessage,
		"workflow_id":   wfID,
	})
	result := buildResult(cmd, resultType, payload)
	if err := env.engine.ApplyResult(context.Background(), result); err != nil {
		t.Fatalf("ApplyResult (failure %s): %v", resultType, err)
	}
	env.resultIDs = append(env.resultIDs, result.MessageID)
}

// ---------------------------------------------------------------------------
// Row-count assertion helpers for the full saga.
// ---------------------------------------------------------------------------

func sagaCounts(t *testing.T, env *sagaEnv, wfID string) map[string]int {
	t.Helper()
	c := map[string]int{}
	// pay_db counts
	c["workflow_instance"] = countRow(t, env.payDB,
		`SELECT count(*) FROM workflow_instance WHERE workflow_id=$1`, wfID)
	c["workflow_action"] = countRow(t, env.payDB,
		`SELECT count(*) FROM workflow_action WHERE workflow_id=$1`, wfID)
	c["outbox"] = countRow(t, env.payDB,
		`SELECT count(*) FROM outbox_message WHERE envelope->>'workflow_id'=$1`, wfID)
	// inbox: count unique result envelope MessageIDs that the engine recorded.
	// We cannot use workflow_action.result_event_id because compensation
	// overwrites it with the compensation result, losing the forward result ID.
	// Instead we count the tracked resultIDs that exist in the inbox.
	if len(env.resultIDs) > 0 {
		c["inbox"] = countRow(t, env.payDB,
			`SELECT count(*) FROM inbox_message WHERE consumer='workflow' AND message_id = ANY($1::uuid[])`,
			pgUUIDArray(env.resultIDs))
	} else {
		c["inbox"] = 0
	}
	// risk_db counts
	c["authorization"] = countRow(t, env.riskDB,
		`SELECT count(*) FROM payment_authorization WHERE workflow_id=$1`, wfID)
	// core_db counts
	c["hold"] = countRow(t, env.coreDB,
		`SELECT count(*) FROM funds_hold WHERE workflow_id=$1`, wfID)
	c["voucher"] = countRow(t, env.coreDB,
		`SELECT count(*) FROM held_transfer WHERE idempotency_key LIKE $1`, "wf:"+wfID+"%")
	return c
}

// pgUUIDArray converts a string slice to a Postgres UUID array literal.
func pgUUIDArray(ids []string) string {
	return "{" + strings.Join(ids, ",") + "}"
}

// ---------------------------------------------------------------------------
// Scenario 1: Happy Path — workflow succeeds end-to-end.
// ---------------------------------------------------------------------------

func TestSaga_HappyPath_Succeeds(t *testing.T) {
	env := setupSagaEnv(t)
	defer env.close()

	const (
		wfID   = "wf-saga-happy"
		custID = "C-SAGA-HAPPY"
		payer  = "SAGA-PAYER-1"
		payee  = "SAGA-PAYEE-1"
	)
	cleanupSaga(t, env, wfID)
	cleanupAccounts(t, env.coreDB, payer, payee)
	env.riskDB.ExecContext(context.Background(), "DELETE FROM blacklist WHERE cust_id=$1", custID)

	// Seed: happy customer + active accounts with balance.
	env.customers[custID] = serviceclient.Customer{
		CustomerID: custID, Status: "active", KYCStatus: "verified",
	}
	seedAccount(t, env.coreDB, payer, custID, 10000.00) // 10000 yuan
	seedAccount(t, env.coreDB, payee, custID, 0.00)

	// Stub account snapshots for the Preparation.
	stubAccts := env.engine // placeholder to access the registry's preparation
	_ = stubAccts
	// We need to inject the account stubs into the Preparation. Since the
	// Preparation was wired with env.customers (shared stub), and the AccountReader
	// is a separate stub, we must rebuild the definition with populated account data.
	prepAccounts := stubAccountReader{
		payer: {AccountNo: payer, CustomerID: custID, Currency: "CNY", Status: "active",
			LedgerBalanceMinor: 10000 * 100, AvailableBalanceMinor: 10000 * 100},
		payee: {AccountNo: payee, CustomerID: custID, Currency: "CNY", Status: "active"},
	}
	preparation := workflows.NewPreparation(env.customers, prepAccounts)
	registry := workflow.NewRegistry()
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(env.store, registry, workflow.EngineConfig{})

	input, _ := json.Marshal(workflows.PrepareInput{
		PaymentID:       "pay-happy",
		PayerCustomerID: custID,
		PayerAccountNo:  payer,
		PayeeAccountNo:  payee,
		Currency:        "CNY",
		AmountMinor:     5000, // 50.00 yuan
	})
	ctx := context.Background()
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1, Input: input,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	// Drive all three actions.
	advance(t, env, wfID) // authorize → succeeded
	advance(t, env, wfID) // hold      → succeeded
	advance(t, env, wfID) // transfer  → succeeded

	// Assert workflow status.
	if s := workflowStatus(t, env.payDB, wfID); s != "succeeded" {
		t.Fatalf("workflow status = %q, want succeeded", s)
	}

	// Assert exact row counts.
	c := sagaCounts(t, env, wfID)
	assertCount(t, "workflow_instance", c["workflow_instance"], 1)
	assertCount(t, "workflow_action", c["workflow_action"], 3)
	assertCount(t, "authorization", c["authorization"], 1)
	assertCount(t, "hold", c["hold"], 1)
	assertCount(t, "voucher", c["voucher"], 1)
	assertCount(t, "outbox", c["outbox"], 3)
	assertCount(t, "inbox", c["inbox"], 3)

	// Verify authorization status = authorized.
	var authStatus string
	env.riskDB.QueryRowContext(ctx,
		`SELECT status FROM payment_authorization WHERE workflow_id=$1`, wfID).Scan(&authStatus)
	if authStatus != "authorized" {
		t.Errorf("authorization status = %q, want authorized", authStatus)
	}
	// Verify hold status = captured.
	var holdStatus string
	env.coreDB.QueryRowContext(ctx,
		`SELECT status FROM funds_hold WHERE workflow_id=$1`, wfID).Scan(&holdStatus)
	if holdStatus != "captured" {
		t.Errorf("hold status = %q, want captured", holdStatus)
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: Risk Rejection — no hold, no voucher.
// ---------------------------------------------------------------------------

func TestSaga_RiskRejection_NoHoldNoVoucher(t *testing.T) {
	env := setupSagaEnv(t)
	defer env.close()

	const (
		wfID   = "wf-saga-reject"
		custID = "C-SAGA-REJECT"
		payer  = "SAGA-PAYER-2"
		payee  = "SAGA-PAYEE-2"
	)
	cleanupSaga(t, env, wfID)
	cleanupAccounts(t, env.coreDB, payer, payee)

	// Seed: customer with high-risk tag → policy rejects.
	env.customers[custID] = serviceclient.Customer{
		CustomerID: custID, Status: "active", KYCStatus: "verified", RiskTags: []string{"high-risk"},
	}
	seedAccount(t, env.coreDB, payer, custID, 10000.00)
	seedAccount(t, env.coreDB, payee, custID, 0.00)

	prepAccounts := stubAccountReader{
		payer: {AccountNo: payer, CustomerID: custID, Currency: "CNY", Status: "active",
			LedgerBalanceMinor: 10000 * 100, AvailableBalanceMinor: 10000 * 100},
		payee: {AccountNo: payee, CustomerID: custID, Currency: "CNY", Status: "active"},
	}
	preparation := workflows.NewPreparation(env.customers, prepAccounts)
	registry := workflow.NewRegistry()
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(env.store, registry, workflow.EngineConfig{})

	input, _ := json.Marshal(workflows.PrepareInput{
		PaymentID: "pay-reject", PayerCustomerID: custID,
		PayerAccountNo: payer, PayeeAccountNo: payee, Currency: "CNY", AmountMinor: 5000,
	})
	ctx := context.Background()
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1, Input: input,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	// Drive authorize → rejected.
	advance(t, env, wfID)

	// Risk rejection is terminal → compensation, but action 0 is the first
	// action (nothing to undo) → engine goes straight to compensated.
	if s := workflowStatus(t, env.payDB, wfID); s != "compensated" {
		t.Fatalf("workflow status = %q, want compensated", s)
	}

	c := sagaCounts(t, env, wfID)
	assertCount(t, "workflow_instance", c["workflow_instance"], 1)
	assertCount(t, "workflow_action", c["workflow_action"], 1)
	assertCount(t, "authorization", c["authorization"], 1)
	assertCount(t, "hold", c["hold"], 0)
	assertCount(t, "voucher", c["voucher"], 0)
	assertCount(t, "outbox", c["outbox"], 1)
	assertCount(t, "inbox", c["inbox"], 1)

	// Authorization status should be rejected.
	var authStatus string
	env.riskDB.QueryRowContext(ctx,
		`SELECT status FROM payment_authorization WHERE workflow_id=$1`, wfID).Scan(&authStatus)
	if authStatus != "rejected" {
		t.Errorf("authorization status = %q, want rejected", authStatus)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: Insufficient Funds — authorization voided by compensation.
// ---------------------------------------------------------------------------

func TestSaga_InsufficientFunds_AuthorizationVoided(t *testing.T) {
	env := setupSagaEnv(t)
	defer env.close()

	const (
		wfID   = "wf-saga-insuff"
		custID = "C-SAGA-INSUFF"
		payer  = "SAGA-PAYER-3"
		payee  = "SAGA-PAYEE-3"
	)
	cleanupSaga(t, env, wfID)
	cleanupAccounts(t, env.coreDB, payer, payee)
	env.riskDB.ExecContext(context.Background(), "DELETE FROM blacklist WHERE cust_id=$1", custID)

	// Seed: valid customer (risk passes) but 0 balance (hold fails).
	env.customers[custID] = serviceclient.Customer{
		CustomerID: custID, Status: "active", KYCStatus: "verified",
	}
	seedAccount(t, env.coreDB, payer, custID, 0.00) // zero balance → insufficient
	seedAccount(t, env.coreDB, payee, custID, 0.00)

	prepAccounts := stubAccountReader{
		payer: {AccountNo: payer, CustomerID: custID, Currency: "CNY", Status: "active"},
		payee: {AccountNo: payee, CustomerID: custID, Currency: "CNY", Status: "active"},
	}
	preparation := workflows.NewPreparation(env.customers, prepAccounts)
	registry := workflow.NewRegistry()
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(env.store, registry, workflow.EngineConfig{})

	input, _ := json.Marshal(workflows.PrepareInput{
		PaymentID: "pay-insuff", PayerCustomerID: custID,
		PayerAccountNo: payer, PayeeAccountNo: payee, Currency: "CNY", AmountMinor: 5000,
	})
	ctx := context.Background()
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1, Input: input,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	// Step 1: authorize succeeds.
	advance(t, env, wfID)
	// Step 2: hold fails (insufficient funds) — the driveHold helper returns a
	// core.hold-failed.v1 envelope with error_class=business_rejected.
	advance(t, env, wfID)
	// Terminal failure → compensation begins. The last succeeded action (0:
	// AuthorizeRisk) is compensated: engine dispatches void-authorization.
	// Step 3: void authorization (compensation result).
	advance(t, env, wfID)

	if s := workflowStatus(t, env.payDB, wfID); s != "compensated" {
		t.Fatalf("workflow status = %q, want compensated", s)
	}

	c := sagaCounts(t, env, wfID)
	assertCount(t, "workflow_instance", c["workflow_instance"], 1)
	assertCount(t, "workflow_action", c["workflow_action"], 2)
	assertCount(t, "authorization", c["authorization"], 1)
	assertCount(t, "hold", c["hold"], 0)
	assertCount(t, "voucher", c["voucher"], 0)
	assertCount(t, "outbox", c["outbox"], 3)
	assertCount(t, "inbox", c["inbox"], 3)

	// Authorization must be voided (not authorized).
	var authStatus string
	env.riskDB.QueryRowContext(ctx,
		`SELECT status FROM payment_authorization WHERE workflow_id=$1`, wfID).Scan(&authStatus)
	if authStatus != "voided" {
		t.Errorf("authorization status = %q, want voided", authStatus)
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: Duplicate Delivery — identical command + result envelopes twice.
// Asserts exactly one effect (Inbox dedup + semantic idempotency).
// ---------------------------------------------------------------------------

func TestSaga_DuplicateDelivery_OneEffect(t *testing.T) {
	env := setupSagaEnv(t)
	defer env.close()

	const (
		wfID   = "wf-saga-dup"
		custID = "C-SAGA-DUP"
		payer  = "SAGA-PAYER-4"
		payee  = "SAGA-PAYEE-4"
	)
	cleanupSaga(t, env, wfID)
	cleanupAccounts(t, env.coreDB, payer, payee)
	env.riskDB.ExecContext(context.Background(), "DELETE FROM blacklist WHERE cust_id=$1", custID)

	env.customers[custID] = serviceclient.Customer{
		CustomerID: custID, Status: "active", KYCStatus: "verified",
	}
	seedAccount(t, env.coreDB, payer, custID, 10000.00)
	seedAccount(t, env.coreDB, payee, custID, 0.00)

	prepAccounts := stubAccountReader{
		payer: {AccountNo: payer, CustomerID: custID, Currency: "CNY", Status: "active",
			LedgerBalanceMinor: 10000 * 100, AvailableBalanceMinor: 10000 * 100},
		payee: {AccountNo: payee, CustomerID: custID, Currency: "CNY", Status: "active"},
	}
	preparation := workflows.NewPreparation(env.customers, prepAccounts)
	registry := workflow.NewRegistry()
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(env.store, registry, workflow.EngineConfig{})

	input, _ := json.Marshal(workflows.PrepareInput{
		PaymentID: "pay-dup", PayerCustomerID: custID,
		PayerAccountNo: payer, PayeeAccountNo: payee, Currency: "CNY", AmountMinor: 5000,
	})
	ctx := context.Background()
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1, Input: input,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	// --- DUPLICATE COMMAND: call the risk service twice with the same command ---
	cmd := readLastCommand(t, env.payDB, wfID)
	// First call creates the authorization.
	result1, err := env.riskSvc.AuthorizePayment(ctx, env.riskDB, riskservice.AuthorizeCommand{
		AuthorizationID: "authz:" + wfID,
		WorkflowID:      wfID,
		IdempotencyKey:  cmd.env.IdempotencyKey,
		CustomerID:      custID,
		AmountCents:     5000,
		Currency:        "CNY",
	})
	if err != nil {
		t.Fatalf("first AuthorizePayment: %v", err)
	}
	// Second call — identical idempotency key → Duplicate=true, no new row.
	result2, err := env.riskSvc.AuthorizePayment(ctx, env.riskDB, riskservice.AuthorizeCommand{
		AuthorizationID: "authz:" + wfID,
		WorkflowID:      wfID,
		IdempotencyKey:  cmd.env.IdempotencyKey,
		CustomerID:      custID,
		AmountCents:     5000,
		Currency:        "CNY",
	})
	if err != nil {
		t.Fatalf("second AuthorizePayment: %v", err)
	}
	if !result2.Duplicate {
		t.Error("second AuthorizePayment should return Duplicate=true")
	}

	// --- DUPLICATE RESULT: feed the same result envelope to ApplyResult twice ---
	payload, _ := json.Marshal(map[string]string{"authorization_id": "authz:" + wfID})
	resultEnv := buildResult(cmd, result1.EventType, payload)
	if err := env.engine.ApplyResult(ctx, resultEnv); err != nil {
		t.Fatalf("first ApplyResult: %v", err)
	}
	revBefore := workflowStatus(t, env.payDB, wfID)
	// Second delivery — SAME MessageID → Inbox dedup, true no-op.
	if err := env.engine.ApplyResult(ctx, resultEnv); err != nil {
		t.Fatalf("second (duplicate) ApplyResult: %v", err)
	}

	// Workflow status must be unchanged (duplicate had no effect).
	if s := workflowStatus(t, env.payDB, wfID); s != revBefore {
		t.Errorf("status changed after duplicate: %q → %q (should be unchanged)", revBefore, s)
	}

	// Exactly ONE authorization row (not two).
	authCount := countRow(t, env.riskDB,
		`SELECT count(*) FROM payment_authorization WHERE workflow_id=$1`, wfID)
	assertCount(t, "authorization (duplicate service call)", authCount, 1)

	// Exactly ONE inbox row for this result envelope (not two).
	inboxCount := countRow(t, env.payDB,
		`SELECT count(*) FROM inbox_message WHERE consumer='workflow' AND message_id=$1`,
		resultEnv.MessageID)
	assertCount(t, "inbox (duplicate result delivery)", inboxCount, 1)

	// Workflow advanced to action 1 (not 2) — current_action=1.
	var currentAction int
	env.payDB.QueryRowContext(ctx,
		`SELECT current_action FROM workflow_instance WHERE workflow_id=$1`, wfID).Scan(&currentAction)
	if currentAction != 1 {
		t.Errorf("current_action = %d, want 1 (single advance)", currentAction)
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: Compensation — failure after two succeeded actions triggers a
// reverse-order compensation walk (release hold → void authorization).
// ---------------------------------------------------------------------------

func TestSaga_Compensation_ReverseWalk(t *testing.T) {
	env := setupSagaEnv(t)
	defer env.close()

	const (
		wfID   = "wf-saga-comp"
		custID = "C-SAGA-COMP"
		payer  = "SAGA-PAYER-5"
		payee  = "SAGA-PAYEE-5"
	)
	cleanupSaga(t, env, wfID)
	cleanupAccounts(t, env.coreDB, payer, payee)
	env.riskDB.ExecContext(context.Background(), "DELETE FROM blacklist WHERE cust_id=$1", custID)

	env.customers[custID] = serviceclient.Customer{
		CustomerID: custID, Status: "active", KYCStatus: "verified",
	}
	seedAccount(t, env.coreDB, payer, custID, 10000.00)
	seedAccount(t, env.coreDB, payee, custID, 0.00)

	prepAccounts := stubAccountReader{
		payer: {AccountNo: payer, CustomerID: custID, Currency: "CNY", Status: "active",
			LedgerBalanceMinor: 10000 * 100, AvailableBalanceMinor: 10000 * 100},
		payee: {AccountNo: payee, CustomerID: custID, Currency: "CNY", Status: "active"},
	}
	preparation := workflows.NewPreparation(env.customers, prepAccounts)
	registry := workflow.NewRegistry()
	if err := registry.Register(workflows.NewPaymentTransferDefinition(preparation)); err != nil {
		t.Fatal(err)
	}
	engine := workflow.NewEngine(env.store, registry, workflow.EngineConfig{})

	input, _ := json.Marshal(workflows.PrepareInput{
		PaymentID: "pay-comp", PayerCustomerID: custID,
		PayerAccountNo: payer, PayeeAccountNo: payee, Currency: "CNY", AmountMinor: 5000,
	})
	ctx := context.Background()
	if _, err := engine.Start(ctx, workflow.StartRequest{
		WorkflowID: wfID, Type: "payment-transfer", Version: 1, Input: input,
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Prepare(ctx, wfID); err != nil {
		t.Fatal(err)
	}

	// Steps 1-2: authorize + hold succeed (creates auth + hold).
	advance(t, env, wfID) // authorize → succeeded
	advance(t, env, wfID) // hold      → succeeded
	// Step 3: transfer FAILS (business_rejected). We inject the failure
	// directly rather than calling the service, simulating a downstream
	// ledger-posting failure. This triggers terminal compensation.
	injectFailure(t, env, wfID, "core.transfer-failed.v1", "business_rejected", "ledger posting failed")
	// Step 4: compensate PlaceFundsHold — engine dispatches release-hold.
	advance(t, env, wfID) // release hold (compensation)
	// Step 5: compensate AuthorizeRisk — engine dispatches void-auth.
	advance(t, env, wfID) // void authorization (compensation)

	if s := workflowStatus(t, env.payDB, wfID); s != "compensated" {
		t.Fatalf("workflow status = %q, want compensated", s)
	}

	c := sagaCounts(t, env, wfID)
	assertCount(t, "workflow_instance", c["workflow_instance"], 1)
	assertCount(t, "workflow_action", c["workflow_action"], 3)
	assertCount(t, "authorization", c["authorization"], 1)
	assertCount(t, "hold", c["hold"], 1)
	assertCount(t, "voucher", c["voucher"], 0) // transfer failed → no voucher
	assertCount(t, "outbox", c["outbox"], 5)
	assertCount(t, "inbox", c["inbox"], 5)

	// Authorization voided by compensation.
	var authStatus string
	env.riskDB.QueryRowContext(ctx,
		`SELECT status FROM payment_authorization WHERE workflow_id=$1`, wfID).Scan(&authStatus)
	if authStatus != "voided" {
		t.Errorf("authorization status = %q, want voided", authStatus)
	}
	// Hold released by compensation.
	var holdStatus string
	env.coreDB.QueryRowContext(ctx,
		`SELECT status FROM funds_hold WHERE workflow_id=$1`, wfID).Scan(&holdStatus)
	if holdStatus != "released" {
		t.Errorf("hold status = %q, want released", holdStatus)
	}

	// Verify reverse-order compensation: action 0 and 1 should be in
	// compensation direction, action 2 should be forward/failed.
	type actRow struct {
		idx       int
		direction string
		status    string
	}
	rows, err := env.payDB.QueryContext(ctx, `
		SELECT action_index, direction, status FROM workflow_action
		WHERE workflow_id=$1 ORDER BY action_index`, wfID)
	if err != nil {
		t.Fatal(err)
	}
	var acts []actRow
	for rows.Next() {
		var a actRow
		rows.Scan(&a.idx, &a.direction, &a.status)
		acts = append(acts, a)
	}
	rows.Close()
	if len(acts) != 3 {
		t.Fatalf("expected 3 action rows, got %d", len(acts))
	}
	// Action 0 (AuthorizeRisk): compensated by void.
	if acts[0].direction != "compensation" || acts[0].status != "compensated" {
		t.Errorf("action 0: direction=%q status=%q, want compensation/compensated", acts[0].direction, acts[0].status)
	}
	// Action 1 (PlaceFundsHold): compensated by release.
	if acts[1].direction != "compensation" || acts[1].status != "compensated" {
		t.Errorf("action 1: direction=%q status=%q, want compensation/compensated", acts[1].direction, acts[1].status)
	}
	// Action 2 (PostLedgerTransfer): forward, failed.
	if acts[2].direction != "forward" || acts[2].status != "failed" {
		t.Errorf("action 2: direction=%q status=%q, want forward/failed", acts[2].direction, acts[2].status)
	}
}
