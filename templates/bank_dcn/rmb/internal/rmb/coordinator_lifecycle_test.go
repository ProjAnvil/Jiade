package rmb

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"bank-dcn/lib/contracts"
	"bank-dcn/lib/sqltest"
)

const stepsSelect = "step_no, dcn, action, status, payload"

func receipt(t *testing.T, rc contracts.Receipt) []byte {
	t.Helper()
	b, _ := json.Marshal(rc)
	return b
}

// handleReceipt drops malformed payloads and receipts for unknown steps without side effects.
func TestHandleReceiptDropsUnknownOrMalformed(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}}, // no rows -> unknown step
	)
	c := &Coordinator{db: db}
	if err := c.handleReceipt([]byte("not json")); err != nil {
		t.Fatalf("malformed receipt must be dropped, got %v", err)
	}
	if err := c.handleReceipt(receipt(t, contracts.Receipt{TxID: "tx-1", StepNo: 1, Status: "DONE"})); err != nil {
		t.Fatalf("unknown step must be dropped, got %v", err)
	}
	if rec.ExecCount("") != 0 {
		t.Fatal("no state may change for malformed or unknown receipts")
	}
}

// handleReceipt: a DONE receipt for a PENDING step advances an all-DONE transaction to COMMITTED.
func TestHandleReceiptDoneAdvancesToCommitted(t *testing.T) {
	p1 := stepPayload(t, "t5", 1, "DEBIT")
	p2 := stepPayload(t, "t5", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"PENDING"}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'DONE'"},
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "DONE", p2},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	if err := c.handleReceipt(receipt(t, contracts.Receipt{TxID: "t5", StepNo: 1, DCN: "dcn01", Status: "DONE"})); err != nil {
		t.Fatal(err)
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMMITTED" {
		t.Fatalf("terminal transition = %+v, want COMMITTED", upd)
	}
}

// handleReceipt: a FAILED receipt triggers reverse compensation for the DONE original step.
func TestHandleReceiptFailedStepStartsCompensation(t *testing.T) {
	p1 := stepPayload(t, "t6", 1, "DEBIT")
	p2 := stepPayload(t, "t6", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"PENDING"}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'FAILED'"},
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "FAILED", p2},
			}},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
		sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
			Rows: [][]any{{int64(3), "dcn01", stepPayload(t, "t6", 3, "COMPENSATE_DEBIT")}}},
	)
	var published int
	c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
		published++
		return nil
	}}
	if err := c.handleReceipt(receipt(t, contracts.Receipt{
		TxID: "t6", StepNo: 2, DCN: "dcn02", Status: "FAILED", Reason: "insufficient funds",
	})); err != nil {
		t.Fatal(err)
	}
	ins := rec.LastExec("INSERT INTO tx_step_log")
	if ins == nil || ins.Args[3] != "COMPENSATE_DEBIT" {
		t.Fatalf("compensation insert = %+v, want COMPENSATE_DEBIT", ins)
	}
	if published != 1 {
		t.Fatalf("compensation published %d times, want 1", published)
	}
}

// handleReceipt: a failing step-status UPDATE propagates the error (receipt is redelivered).
func TestHandleReceiptUpdateErrorPropagates(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"PENDING"}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'DONE'", Err: errors.New("update boom")},
	)
	c := &Coordinator{db: db}
	err := c.handleReceipt(receipt(t, contracts.Receipt{TxID: "t7", StepNo: 1, Status: "DONE"}))
	if err == nil || !strings.Contains(err.Error(), "update boom") {
		t.Fatalf("err = %v, want update boom", err)
	}
}

// handleReceipt: a late DONE receipt on a tx that is still PROCESSING re-marks the step DONE
// and advances, but does not reopen anything (reopen is only for COMPENSATED transactions).
func TestHandleReceiptLateDoneWithoutReopen(t *testing.T) {
	p1 := stepPayload(t, "t8", 1, "DEBIT")
	p2 := stepPayload(t, "t8", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"FAILED"}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "SET status = 'DONE' WHERE tx_id = ? AND step_no = ? AND status = 'FAILED'"},
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "DONE", p2},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	if err := c.handleReceipt(receipt(t, contracts.Receipt{TxID: "t8", StepNo: 2, Status: "DONE"})); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE tx_log SET status = 'PROCESSING'") != 0 {
		t.Fatal("a PROCESSING tx must not be reopened")
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMMITTED" {
		t.Fatalf("terminal transition = %+v, want COMMITTED", upd)
	}
}

// advance: a transaction already in a terminal state is left untouched.
func TestAdvanceNotProcessingIsNoop(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"COMMITTED"}}},
	)
	c := &Coordinator{db: db}
	if err := c.advance("t9"); err != nil {
		t.Fatal(err)
	}
	if rec.QueryCount(stepsSelect) != 0 {
		t.Fatal("steps must not be loaded for a non-PROCESSING tx")
	}
}

// advance: with a FAILED original but also a PENDING original and nothing to compensate,
// advance waits (the timeout loop settles the PENDING step later).
func TestAdvanceWaitsWhenPendingAndFailedRemain(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "PENDING", stepPayload(t, "ta", 1, "DEBIT")},
				{int64(2), "dcn02", "CREDIT", "FAILED", stepPayload(t, "ta", 2, "CREDIT")},
			}},
	)
	c := &Coordinator{db: db}
	if err := c.advance("ta"); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE tx_log") != 0 || rec.ExecCount("INSERT INTO tx_step_log") != 0 {
		t.Fatal("advance must wait, not transition or compensate")
	}
}

// advance: all originals FAILED with nothing DONE goes straight to COMPENSATED (empty compensation).
func TestAdvanceAllFailedWithNothingDoneGoesCompensated(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "FAILED", stepPayload(t, "tb", 1, "DEBIT")},
				{int64(2), "dcn02", "CREDIT", "FAILED", stepPayload(t, "tb", 2, "CREDIT")},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	if err := c.advance("tb"); err != nil {
		t.Fatal(err)
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMPENSATED" {
		t.Fatalf("terminal transition = %+v, want COMPENSATED", upd)
	}
}

// advance: a failing steps query surfaces the error.
func TestAdvanceStepsQueryError(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect, Err: errors.New("steps boom")},
	)
	c := &Coordinator{db: db}
	if err := c.advance("tc"); err == nil || !strings.Contains(err.Error(), "steps boom") {
		t.Fatalf("err = %v, want steps boom", err)
	}
}

// advance: a corrupted original payload makes compensation construction fail.
func TestAdvanceBadCompensationPayloadFails(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", "not-json"},
				{int64(2), "dcn02", "CREDIT", "FAILED", stepPayload(t, "td", 2, "CREDIT")},
			}},
	)
	c := &Coordinator{db: db}
	if err := c.advance("td"); err == nil {
		t.Fatal("bad payload must fail compensation construction")
	}
	if rec.ExecCount("INSERT INTO tx_step_log") != 0 {
		t.Fatal("no compensation step may be inserted from a bad payload")
	}
}

// advance: a FAILED compensation step within the 3-attempt budget is reset to PENDING
// and redelivered instead of failing the whole transaction.
func TestAdvanceRetriesFailedCompensationStep(t *testing.T) {
	comp := stepPayload(t, "te", 3, "COMPENSATE_DEBIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "te", 1, "DEBIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "FAILED", comp},
			}},
		sqltest.Rule{Contains: "SET status = 'PENDING' WHERE tx_id = ? AND step_no = ?"},
		sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
			Rows: [][]any{{int64(3), "dcn01", comp}}},
	)
	var published int
	c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
		published++
		return nil
	}}
	if err := c.advance("te"); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("SET status = 'PENDING'") != 1 {
		t.Fatal("failed compensation step must be reset to PENDING")
	}
	if published != 1 {
		t.Fatalf("compensation redelivered %d times, want 1", published)
	}
	if rec.ExecCount("UPDATE tx_log SET status") != 0 {
		t.Fatal("a retried compensation must not terminate the tx")
	}
}

// advance: once the 3-attempt budget is exhausted the transaction terminates as FAILED.
func TestAdvanceCompensationRetryExhaustedFailsTx(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "tf", 1, "DEBIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "FAILED", stepPayload(t, "tf", 3, "COMPENSATE_DEBIT")},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	c.attempts.Store("tf:3", 3) // retry budget already spent
	if err := c.advance("tf"); err != nil {
		t.Fatal(err)
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "FAILED" {
		t.Fatalf("terminal transition = %+v, want FAILED", upd)
	}
}

// advance: a failing retry UPDATE returns the error so the receipt is redelivered.
func TestAdvanceCompensationRetryUpdateError(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "tg", 1, "DEBIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "FAILED", stepPayload(t, "tg", 3, "COMPENSATE_DEBIT")},
			}},
		sqltest.Rule{Contains: "SET status = 'PENDING' WHERE tx_id = ? AND step_no = ?",
			Err: errors.New("retry boom")},
	)
	c := &Coordinator{db: db}
	if err := c.advance("tg"); err == nil || !strings.Contains(err.Error(), "retry boom") {
		t.Fatalf("err = %v, want retry boom", err)
	}
}

// advance: a PENDING compensation step means advance waits for its receipt.
func TestAdvanceWaitsForPendingCompensationReceipts(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "th", 1, "DEBIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "PENDING", stepPayload(t, "th", 3, "COMPENSATE_DEBIT")},
			}},
	)
	c := &Coordinator{db: db}
	if err := c.advance("th"); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE tx_log") != 0 {
		t.Fatal("advance must wait for the compensation receipt")
	}
}

// advance: a failing terminal-status UPDATE is logged inside transition but does not fail advance.
func TestAdvanceToleratesTransitionFailure(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows:    [][]any{{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "ti", 1, "DEBIT")}}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status", Err: errors.New("transition boom")},
	)
	c := &Coordinator{db: db}
	if err := c.advance("ti"); err != nil {
		t.Fatalf("transition failure must be tolerated, got %v", err)
	}
	if rec.ExecCount("UPDATE tx_log SET status") != 1 {
		t.Fatal("the terminal UPDATE must have been attempted")
	}
}

// reopenCompensation: re-open success, concurrent-already-reopened (0 rows), and update failure.
func TestReopenCompensation(t *testing.T) {
	cases := []struct {
		name    string
		result  driver.Result
		err     error
		wantErr bool
	}{
		{"reopened", result1(), nil, false},
		{"already reopened concurrently", aff(0), nil, false},
		{"update error", nil, errors.New("reopen boom"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rule := sqltest.Rule{Contains: "UPDATE tx_log SET status = 'PROCESSING'"}
			rule.Result, rule.Err = tc.result, tc.err
			db, rec := sqltest.NewDB(t, rule)
			c := &Coordinator{db: db}
			err := c.reopenCompensation("tx-1")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if rec.ExecCount("UPDATE tx_log SET status = 'PROCESSING'") != 1 {
				t.Fatal("reopen UPDATE must run exactly once")
			}
		})
	}
}

// compensatePayload: bad payloads fail; valid ones reverse the action and renumber the step.
func TestCompensatePayload(t *testing.T) {
	if _, err := compensatePayload(stepRow{payload: "not-json"}, 2, "COMPENSATE_DEBIT"); err == nil {
		t.Fatal("bad payload must fail")
	}
	out, err := compensatePayload(stepRow{payload: stepPayload(t, "tj", 1, "DEBIT")}, 2, "COMPENSATE_DEBIT")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stepNo":2`, "COMPENSATE_DEBIT", `"accountId":1001`} {
		if !strings.Contains(out, want) {
			t.Errorf("payload %s missing %s", out, want)
		}
	}
}

// loadSteps: query and scan failures surface as errors.
func TestLoadSteps(t *testing.T) {
	t.Run("query error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: stepsSelect, Err: errors.New("load boom")})
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := loadSteps(tx, "tk"); err == nil || !strings.Contains(err.Error(), "load boom") {
			t.Fatalf("err = %v, want load boom", err)
		}
	})
	t.Run("scan error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: stepsSelect,
				Columns: []string{"step_no", "dcn", "action", "status", "payload"},
				Rows:    [][]any{{"not-a-number", "dcn01", "DEBIT", "DONE", "p"}}},
		)
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := loadSteps(tx, "tk"); err == nil {
			t.Fatal("unscannable row must fail")
		}
	})
}

// expire: a timed-out PROCESSING tx with no compensation steps gets its PENDING steps marked
// FAILED, then advance settles the empty compensation to COMPENSATED.
func TestExpireMarksPendingFailedAndSettles(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "COUNT(*) FROM tx_step_log", Columns: []string{"n"}, Rows: [][]any{{int64(0)}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'FAILED'"},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows:    [][]any{{int64(1), "dcn01", "DEBIT", "FAILED", stepPayload(t, "tm", 1, "DEBIT")}}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	c.expire("tm")
	if rec.ExecCount("UPDATE tx_step_log SET status = 'FAILED'") != 1 {
		t.Fatal("pending steps must be marked FAILED on timeout")
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMPENSATED" {
		t.Fatalf("terminal transition = %+v, want COMPENSATED", upd)
	}
}

// expire: a tx already compensating is not timed out; stranded PENDING compensation steps
// are only redelivered.
func TestExpireDuringCompensationRedeliversInsteadOfFailing(t *testing.T) {
	comp := stepPayload(t, "tn", 3, "COMPENSATE_DEBIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "COUNT(*) FROM tx_step_log", Columns: []string{"n"}, Rows: [][]any{{int64(1)}}},
		sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
			Rows: [][]any{{int64(3), "dcn01", comp}}},
	)
	var published int
	c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
		published++
		return nil
	}}
	c.expire("tn")
	if published != 1 {
		t.Fatalf("pending compensation redelivered %d times, want 1", published)
	}
	if rec.ExecCount("UPDATE tx_step_log SET status = 'FAILED'") != 0 {
		t.Fatal("a compensating tx must not be timed out")
	}
}

// expire: non-PROCESSING transactions and infrastructure errors are skipped without side effects.
func TestExpireSkipsAdverseStates(t *testing.T) {
	t.Run("terminal tx is skipped", func(t *testing.T) {
		db, rec := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"COMMITTED"}}},
		)
		c := &Coordinator{db: db}
		c.expire("to")
		if rec.QueryCount("COUNT(*)") != 0 {
			t.Fatal("a terminal tx must not be inspected further")
		}
	})
	t.Run("lock query error is skipped", func(t *testing.T) {
		db, rec := sqltest.NewDB(t, sqltest.Rule{Contains: "FOR UPDATE", Err: errors.New("lock boom")})
		c := &Coordinator{db: db}
		c.expire("to")
		if rec.QueryCount("COUNT(*)") != 0 {
			t.Fatal("no further work after a lock failure")
		}
	})
	t.Run("count query error is skipped", func(t *testing.T) {
		db, rec := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
			sqltest.Rule{Contains: "COUNT(*) FROM tx_step_log", Err: errors.New("count boom")},
		)
		c := &Coordinator{db: db}
		c.expire("to")
		if rec.ExecCount("UPDATE tx_step_log SET status = 'FAILED'") != 0 {
			t.Fatal("no steps may be failed after a count error")
		}
	})
	t.Run("step update error is skipped", func(t *testing.T) {
		db, rec := sqltest.NewDB(t,
			sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
			sqltest.Rule{Contains: "COUNT(*) FROM tx_step_log", Columns: []string{"n"}, Rows: [][]any{{int64(0)}}},
			sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'FAILED'", Err: errors.New("update boom")},
		)
		c := &Coordinator{db: db}
		c.expire("to")
		if rec.QueryCount(stepsSelect) != 0 {
			t.Fatal("advance must not run after a failed step update")
		}
	})
}

// recover: every unfinished tx gets its PENDING steps redelivered; load failures and
// unscannable ids are tolerated.
func TestRecover(t *testing.T) {
	t.Run("redelivers pending steps per unfinished tx", func(t *testing.T) {
		p := stepPayload(t, "tr1", 1, "DEBIT")
		db, rec := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'",
				Columns: []string{"tx_id"}, Rows: [][]any{{"tr1"}, {"tr2"}}},
			sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
				Rows: [][]any{{int64(1), "dcn01", p}}},
		)
		var published int
		c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
			published++
			return nil
		}}
		c.recover()
		if published != 2 {
			t.Fatalf("redelivered %d times, want one per unfinished tx", published)
		}
		if rec.QueryCount("status = 'PENDING'") != 2 {
			t.Fatal("pending steps must be loaded once per tx")
		}
	})
	t.Run("scan error is tolerated", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'",
				Columns: []string{"tx_id"}, Rows: [][]any{{int64(12345)}}}, // not a string
		)
		c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
			t.Error("an unscannable tx id must not be recovered")
			return nil
		}}
		c.recover()
	})
	t.Run("query error is tolerated", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "SELECT tx_id FROM tx_log WHERE status = 'PROCESSING'",
				Err: errors.New("recover scan boom")},
		)
		c := &Coordinator{db: db}
		c.recover() // must not panic
	})
}

// publishPending: a failing publish is logged and tolerated (steps are redelivered by
// the timeout loop / recover).
func TestPublishFailureIsTolerated(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"},
			Rows: [][]any{{int64(1), "dcn01", stepPayload(t, "tz", 1, "DEBIT")}}},
	)
	c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
		return errors.New("mq down")
	}}
	c.publishPending("tz") // must not panic
}

// handleReceipt: a late DONE whose step UPDATE fails propagates the error (redelivery).
func TestHandleReceiptLateDoneUpdateError(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"FAILED"}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"COMPENSATED"}}},
		sqltest.Rule{Contains: "SET status = 'DONE' WHERE tx_id = ? AND step_no = ? AND status = 'FAILED'",
			Err: errors.New("late update boom")},
	)
	c := &Coordinator{db: db}
	err := c.handleReceipt(receipt(t, contracts.Receipt{TxID: "tq", StepNo: 1, Status: "DONE"}))
	if err == nil || !strings.Contains(err.Error(), "late update boom") {
		t.Fatalf("err = %v, want late update boom", err)
	}
}

// handleReceipt: a late DONE on a COMPENSATED tx whose reopen fails propagates the error.
func TestHandleReceiptReopenError(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"FAILED"}}},
		sqltest.Rule{Contains: txSelect, Columns: []string{"status"}, Rows: [][]any{{"COMPENSATED"}}},
		sqltest.Rule{Contains: "SET status = 'DONE' WHERE tx_id = ? AND step_no = ? AND status = 'FAILED'"},
		sqltest.Rule{Contains: "UPDATE tx_log SET status = 'PROCESSING'", Err: errors.New("reopen boom")},
	)
	c := &Coordinator{db: db}
	err := c.handleReceipt(receipt(t, contracts.Receipt{TxID: "tq", StepNo: 1, Status: "DONE"}))
	if err == nil || !strings.Contains(err.Error(), "reopen boom") {
		t.Fatalf("err = %v, want reopen boom", err)
	}
}

// advance: a failing row lock surfaces the error.
func TestAdvanceLockError(t *testing.T) {
	db, _ := sqltest.NewDB(t, sqltest.Rule{Contains: "FOR UPDATE", Err: errors.New("lock boom")})
	c := &Coordinator{db: db}
	if err := c.advance("tr"); err == nil || !strings.Contains(err.Error(), "lock boom") {
		t.Fatalf("err = %v, want lock boom", err)
	}
}

// advance: a DONE original whose action has no reverse is skipped by compensation building.
func TestAdvanceSkipsNonReversibleAction(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: stepsSelect,
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "TRANSFER", "DONE", stepPayload(t, "ts", 1, "TRANSFER")},
				{int64(2), "dcn02", "CREDIT", "FAILED", stepPayload(t, "ts", 2, "CREDIT")},
			}},
		sqltest.Rule{Contains: "status = 'PENDING'", Columns: []string{"step_no", "dcn", "payload"}},
	)
	var published int
	c := &Coordinator{db: db, publishFn: func(string, string, []byte) error {
		published++
		return nil
	}}
	if err := c.advance("ts"); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("INSERT INTO tx_step_log") != 0 {
		t.Fatal("a non-reversible action must not produce a compensation step")
	}
	if published != 0 {
		t.Fatal("nothing may be published without a new compensation step")
	}
}

// expire: an advance failure after timeout marking is logged, not propagated.
func TestExpireToleratesAdvanceFailure(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "COUNT(*) FROM tx_step_log", Columns: []string{"n"}, Rows: [][]any{{int64(0)}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'FAILED'"},
		sqltest.Rule{Contains: stepsSelect, Err: errors.New("advance boom")}, // advance fails
	)
	c := &Coordinator{db: db}
	c.expire("tt") // must not panic
	if rec.ExecCount("UPDATE tx_step_log SET status = 'FAILED'") != 1 {
		t.Fatal("the timeout marking must still have happened")
	}
}
