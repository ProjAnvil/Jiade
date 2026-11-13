package workflow

// Unit tests for PostgresStore SQL paths using the fakesql harness in
// postgres_fakesql_test.go (no live PostgreSQL). Live-DB round-trips remain in
// the integration-gated postgres_integration_test.go.

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"bank/lib/messaging"
)

var pgTestNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

const pgInstanceCols = "workflow_id,type,definition_version,status,input_json,prepared_context_json,current_action,revision,lease_owner,lease_until,next_wakeup_at,operational_deadline,last_error_class,last_error,correlation_id"

func pgInstanceRow(id string) []driver.Value {
	return []driver.Value{id, "payment-transfer", int64(1), "running", []byte(`{"k":1}`), nil,
		int64(0), int64(3), "owner-1", pgTestNow, pgTestNow, nil, nil, nil, "corr-1"}
}

const pgActionCols = "action_index,name,status,direction,attempt,idempotency_key,command_id,result_event_id,deadline_at,output,last_error_class,last_error,accepted_result_types"

func pgActionRow(idx int64) []driver.Value {
	return []driver.Value{idx, "AuthorizeRisk", "succeeded", "forward", int64(1), "idem-1",
		"cmd-1", "evt-1", pgTestNow, []byte(`{"ok":true}`), nil, nil, []byte(`["risk.payment-authorized.v1"]`)}
}

// storeWithInstance wires a fake DB whose instance+action load queries return
// the scripted rows, then calls fn with the loaded store.
func storeWithInstance(t *testing.T, instanceRows, actionRows [][]driver.Value) (*PostgresStore, *fakeDB) {
	t.Helper()
	db := &fakeDB{}
	db.add(queryPlan{marker: "for update", cols: strings.Split(pgInstanceCols, ","), rows: instanceRows})
	db.add(queryPlan{marker: "from workflow_action", cols: strings.Split(pgActionCols, ","), rows: actionRows})
	return NewPostgresStore(db.open()), db
}

func TestPostgresStore_Create(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into workflow_instance", cols: []string{"workflow_id"}, rows: [][]driver.Value{{"wf-1"}}})
	s := NewPostgresStore(db.open())
	inst, err := s.Create(context.Background(), StartRequest{WorkflowID: "wf-1", Type: "payment-transfer", Version: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ID != "wf-1" || inst.Status != StatusPreparing || inst.Revision != 0 {
		t.Errorf("created instance wrong: %+v", inst)
	}
	if string(inst.Input) != "{}" {
		t.Errorf("empty input must normalize to {}, got %s", inst.Input)
	}
	_, commits, _ := db.counters()
	if commits != 1 {
		t.Errorf("commits = %d, want 1", commits)
	}
}

func TestPostgresStore_Create_DuplicateRejected(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into workflow_instance", cols: []string{"workflow_id"}}) // conflict: RETURNING empty
	s := NewPostgresStore(db.open())
	if _, err := s.Create(context.Background(), StartRequest{WorkflowID: "wf-dup"}); !errors.Is(err, ErrInstanceExists) {
		t.Fatalf("want ErrInstanceExists, got %v", err)
	}
}

func TestPostgresStore_Create_InsertErrorWrapped(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into workflow_instance", err: errors.New("syntax error")})
	s := NewPostgresStore(db.open())
	if _, err := s.Create(context.Background(), StartRequest{WorkflowID: "wf-1"}); err == nil || !strings.Contains(err.Error(), "syntax error") {
		t.Fatalf("want wrapped error, got %v", err)
	}
}

func TestPostgresStore_WithInstance_LoadsInstanceAndActions(t *testing.T) {
	s, db := storeWithInstance(t,
		[][]driver.Value{pgInstanceRow("wf-1")},
		[][]driver.Value{pgActionRow(0), pgActionRow(1)})

	var loaded *Instance
	err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		loaded = tx.Instance()
		return nil
	})
	if err != nil {
		t.Fatalf("WithInstance: %v", err)
	}
	if loaded == nil || loaded.ID != "wf-1" || loaded.Status != StatusRunning || loaded.Revision != 3 {
		t.Fatalf("instance mapping wrong: %+v", loaded)
	}
	if loaded.CorrelationID != "corr-1" || loaded.LeaseOwner != "owner-1" {
		t.Errorf("nullable mapping wrong: %+v", loaded)
	}
	if len(loaded.Actions) != 2 {
		t.Fatalf("actions = %d, want 2", len(loaded.Actions))
	}
	a := loaded.Actions[0]
	if a.Name != "AuthorizeRisk" || a.Status != ActionSucceeded || a.CommandID != "cmd-1" || a.ResultEventID != "evt-1" {
		t.Errorf("action mapping wrong: %+v", a)
	}
	if len(a.AcceptedResultTypes) != 1 || a.AcceptedResultTypes[0] != "risk.payment-authorized.v1" {
		t.Errorf("accepted types wrong: %v", a.AcceptedResultTypes)
	}
	_, commits, _ := db.counters()
	if commits != 1 {
		t.Errorf("commits = %d, want 1", commits)
	}
}

func TestPostgresStore_WithInstance_Missing(t *testing.T) {
	s, _ := storeWithInstance(t, nil, nil)
	err := s.WithInstance(context.Background(), "wf-nope", func(Tx) error { return nil })
	if !errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("want ErrInstanceNotFound, got %v", err)
	}
}

func TestPostgresStore_WithInstance_CallbackErrorRollsBack(t *testing.T) {
	boom := errors.New("callback failed")
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	err := s.WithInstance(context.Background(), "wf-1", func(Tx) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("want callback error, got %v", err)
	}
	_, commits, rolls := db.counters()
	if commits != 0 || rolls != 1 {
		t.Errorf("tx counters commit:%d rollback:%d, want 0/1", commits, rolls)
	}
}

func TestPostgresStore_WithInstance_BadAcceptedTypesFails(t *testing.T) {
	bad := pgActionRow(0)
	bad[12] = []byte("{not-json")
	s, _ := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, [][]driver.Value{bad})
	err := s.WithInstance(context.Background(), "wf-1", func(Tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "accepted_result_types") {
		t.Fatalf("want accepted_result_types decode error, got %v", err)
	}
}

func TestPostgresStore_WithInstanceTx_RunsInCallerTx(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	db.add(queryPlan{marker: "update workflow_instance", affected: 1})

	sqlDB := db.open()
	tx, err := sqlDB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var inst *Instance
	err = s.WithInstanceTx(context.Background(), tx, "wf-1", func(t2 Tx) error {
		inst = t2.Instance()
		updated := *t2.Instance()
		updated.Status = StatusSucceeded
		updated.Revision++
		return t2.SaveInstance(updated)
	})
	if err != nil {
		t.Fatalf("WithInstanceTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if inst == nil || inst.ID != "wf-1" {
		t.Fatalf("instance not loaded: %+v", inst)
	}
	if call := db.lastCallWith("update workflow_instance"); call == nil || call.args[2] != "succeeded" {
		t.Errorf("SaveInstance args wrong: %+v", call)
	}
}

func TestPgTx_InsertInbox(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	env := messaging.NewEnvelope("risk.payment-authorized.v1", "corr-1", json.RawMessage(`{}`), nil)

	db.add(queryPlan{marker: "insert into inbox_message", cols: []string{"message_id"}, rows: [][]driver.Value{{env.MessageID}}})
	var inserted bool
	if err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		var err error
		inserted, err = tx.InsertInbox("workflow", env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Error("first insert must report inserted=true")
	}
}

func TestPgTx_InsertInbox_Duplicate(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	env := messaging.NewEnvelope("risk.payment-authorized.v1", "corr-1", json.RawMessage(`{}`), nil)

	db.add(queryPlan{marker: "insert into inbox_message", cols: []string{"message_id"}}) // RETURNING empty
	var inserted bool
	if err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		var err error
		inserted, err = tx.InsertInbox("workflow", env)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Error("duplicate insert must report inserted=false, nil")
	}
}

func TestPgTx_SaveInstance_RevisionConflict(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	db.add(queryPlan{marker: "update workflow_instance", affected: 0}) // stale revision
	err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		updated := *tx.Instance()
		updated.Revision = 99
		return tx.SaveInstance(updated)
	})
	if err == nil || !strings.Contains(err.Error(), "optimistic revision conflict") {
		t.Fatalf("want revision conflict error, got %v", err)
	}
}

func TestPgTx_SaveAction(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	db.add(queryPlan{marker: "insert into workflow_action", affected: 1})
	var inst *Instance
	err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		inst = tx.Instance()
		return tx.SaveAction(ActionRecord{
			Index: 0, Name: "AuthorizeRisk", Status: ActionWaitingResult,
			Direction: "forward", IdempotencyKey: "idem-1", CommandID: "cmd-9",
			AcceptedResultTypes: []string{"risk.payment-authorized.v1", "risk.payment-rejected.v1"},
		})
	})
	if err != nil {
		t.Fatalf("SaveAction: %v", err)
	}
	call := db.lastCallWith("insert into workflow_action")
	if call == nil || call.args[1] != int64(0) || string(call.args[13].([]byte)) != `["risk.payment-authorized.v1","risk.payment-rejected.v1"]` {
		t.Errorf("SaveAction args wrong: %+v", call)
	}
	// Read-your-writes: the Actions slice must reflect the saved record.
	if len(inst.Actions) != 1 || inst.Actions[0].CommandID != "cmd-9" {
		t.Errorf("Actions not updated: %+v", inst.Actions)
	}
}

func TestPgTx_SaveAction_ZeroRowsFails(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	db.add(queryPlan{marker: "insert into workflow_action", affected: 0})
	err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		return tx.SaveAction(ActionRecord{Index: 0, Name: "x"})
	})
	if err == nil || !strings.Contains(err.Error(), "rows affected") {
		t.Fatalf("want rows-affected error, got %v", err)
	}
}

func TestPgTx_AppendOutbox(t *testing.T) {
	s, db := storeWithInstance(t, [][]driver.Value{pgInstanceRow("wf-1")}, nil)
	db.add(queryPlan{marker: "insert into outbox_message", affected: 1})
	env := messaging.NewEnvelope("core.hold-placed.v1", "corr-1", json.RawMessage(`{}`), nil)
	err := s.WithInstance(context.Background(), "wf-1", func(tx Tx) error {
		return tx.AppendOutbox(env, "core.hold.placed")
	})
	if err != nil {
		t.Fatalf("AppendOutbox: %v", err)
	}
	if call := db.lastCallWith("insert into outbox_message"); call == nil || call.args[0] != env.MessageID || call.args[3] != "core.hold.placed" {
		t.Errorf("outbox args wrong: %+v", call)
	}
}

func TestPostgresStore_ClaimRunnable(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "with locked as", cols: []string{"workflow_id"}, rows: [][]driver.Value{{"wf-1"}, {"wf-2"}}})
	s := NewPostgresStore(db.open())
	ids, err := s.ClaimRunnable(context.Background(), "recovery-1", pgTestNow, time.Minute, 10)
	if err != nil {
		t.Fatalf("ClaimRunnable: %v", err)
	}
	if len(ids) != 2 || ids[0] != "wf-1" {
		t.Fatalf("ids = %v", ids)
	}
	if call := db.lastCallWith("update workflow_instance"); call == nil || call.args[2] != "recovery-1" {
		t.Errorf("lease owner not passed: %+v", call)
	}

	// limit<=0 short-circuits without touching the DB.
	none, err := s.ClaimRunnable(context.Background(), "recovery-1", pgTestNow, time.Minute, 0)
	if err != nil || none != nil {
		t.Errorf("limit=0 must be a no-op: %v %v", none, err)
	}
}

func TestPostgresStore_RenewLease(t *testing.T) {
	// Held by us: one row affected.
	db := &fakeDB{}
	db.add(queryPlan{marker: "update workflow_instance", affected: 1})
	s := NewPostgresStore(db.open())
	if err := s.RenewLease(context.Background(), "wf-1", "recovery-1", pgTestNow, time.Minute); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}

	// Held by someone else: zero rows + instance exists.
	held := &fakeDB{}
	held.add(queryPlan{marker: "update workflow_instance", affected: 0})
	held.add(queryPlan{marker: "select 1", cols: []string{"exists"}, rows: [][]driver.Value{{int64(1)}}})
	s2 := NewPostgresStore(held.open())
	if err := s2.RenewLease(context.Background(), "wf-1", "recovery-1", pgTestNow, time.Minute); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("want ErrLeaseNotHeld, got %v", err)
	}

	// Missing instance: zero rows + no existence row.
	missing := &fakeDB{}
	missing.add(queryPlan{marker: "update workflow_instance", affected: 0})
	missing.add(queryPlan{marker: "select 1", cols: []string{"exists"}})
	s3 := NewPostgresStore(missing.open())
	if err := s3.RenewLease(context.Background(), "wf-1", "recovery-1", pgTestNow, time.Minute); !errors.Is(err, ErrInstanceNotFound) {
		t.Fatalf("want ErrInstanceNotFound, got %v", err)
	}
}

func TestPostgresStore_ReleaseLease(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "update workflow_instance", affected: 1})
	s := NewPostgresStore(db.open())
	if err := s.ReleaseLease(context.Background(), "wf-1", "recovery-1"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	fail := &fakeDB{}
	fail.add(queryPlan{marker: "update workflow_instance", err: errors.New("boom")})
	s2 := NewPostgresStore(fail.open())
	if err := s2.ReleaseLease(context.Background(), "wf-1", "recovery-1"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want wrapped error, got %v", err)
	}
}

func TestPostgresStore_TimedOut(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from workflow_instance", cols: []string{"workflow_id"}, rows: [][]driver.Value{{"wf-1"}}})
	s := NewPostgresStore(db.open())
	ids, err := s.TimedOut(context.Background(), pgTestNow, 10)
	if err != nil || len(ids) != 1 || ids[0] != "wf-1" {
		t.Fatalf("TimedOut = %v, %v", ids, err)
	}

	none, err := s.TimedOut(context.Background(), pgTestNow, 0)
	if err != nil || none != nil {
		t.Errorf("limit=0 must be a no-op: %v %v", none, err)
	}
}

func TestPostgresStore_NonTerminalDefinitions(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "select distinct type", cols: []string{"type", "definition_version"}, rows: [][]driver.Value{{"payment-transfer", int64(1)}, {"payment-reversal", int64(1)}}})
	s := NewPostgresStore(db.open())
	refs, err := s.NonTerminalDefinitions(context.Background())
	if err != nil || len(refs) != 2 || refs[0].Type != "payment-transfer" || refs[0].Version != 1 {
		t.Fatalf("NonTerminalDefinitions = %+v, %v", refs, err)
	}
}
