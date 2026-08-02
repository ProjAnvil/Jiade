package rmb

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"

	"dcn/internal/contracts"
	"dcn/internal/platform/sqltest"
)

func stepPayload(t *testing.T, txID string, stepNo int, action string) string {
	t.Helper()
	b, _ := json.Marshal(contracts.StepMessage{
		TxID: txID, StepNo: stepNo, Action: action, AccountID: 1001, Amount: "50.00",
	})
	return string(b)
}

// register：txId 已存在时幂等返回当前状态，不重复落库。
func TestRegisterIdempotent(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT status FROM tx_log", Columns: []string{"status"},
			Rows: [][]any{{"COMMITTED"}}},
	)
	c := &Coordinator{db: db}
	txID, status, err := c.register(txRequest{TxID: "tx-1", Type: "TRANSFER",
		Steps: []txRequestStep{{DCN: "dcn01", Action: "DEBIT", AccountID: 1001, Amount: "50.00"}}})
	if err != nil || txID != "tx-1" || status != "COMMITTED" {
		t.Fatalf("register = (%s, %s, %v)", txID, status, err)
	}
	if rec.ExecCount("INSERT INTO tx_log") != 0 {
		t.Fatal("idempotent register must not insert")
	}
}

// advance：全部 DONE → COMMITTED。
func TestAdvanceAllDoneCommits(t *testing.T) {
	p1 := stepPayload(t, "t1", 1, "DEBIT")
	p2 := stepPayload(t, "t1", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "DONE", p2},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	if err := c.advance("t1"); err != nil {
		t.Fatal(err)
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMMITTED" {
		t.Fatalf("want COMMITTED transition, got %+v", upd)
	}
}

// advance：一步 FAILED → 为 DONE 的原始步骤逆序补建 COMPENSATE_* 并投递。
func TestAdvanceFailedStepCreatesCompensation(t *testing.T) {
	p1 := stepPayload(t, "t2", 1, "DEBIT")
	p2 := stepPayload(t, "t2", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "FAILED", p2},
			}},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
		sqltest.Rule{Contains: "SELECT step_no, dcn, payload",
			Columns: []string{"step_no", "dcn", "payload"},
			Rows:    [][]any{{int64(3), "dcn01", stepPayload(t, "t2", 3, "COMPENSATE_DEBIT")}}},
	)
	var published []string
	c := &Coordinator{db: db, publishFn: func(exchange, key string, body []byte) error {
		published = append(published, string(body))
		return nil
	}}
	if err := c.advance("t2"); err != nil {
		t.Fatal(err)
	}
	ins := rec.LastExec("INSERT INTO tx_step_log")
	if ins == nil || ins.Args[3] != "COMPENSATE_DEBIT" {
		t.Fatalf("want COMPENSATE_DEBIT step inserted, got %+v", ins)
	}
	if len(published) != 1 || !strings.Contains(published[0], "COMPENSATE_DEBIT") {
		t.Fatalf("compensation not published: %v", published)
	}
}

// handleReceipt：迟到 DONE 回执（步骤已 FAILED、事务已 COMPENSATED）→ 重开事务并补发反向补偿。
func TestLateDoneReceiptReopensAndRecompensates(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"FAILED"}}},
		sqltest.Rule{Contains: "SELECT status FROM tx_log WHERE tx_id = ?", Max: 1,
			Columns: []string{"status"}, Rows: [][]any{{"COMPENSATED"}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'DONE'"},
		sqltest.Rule{Contains: "UPDATE tx_log SET status = 'PROCESSING'", Result: result1()},
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "t3", 1, "DEBIT")},
				{int64(2), "dcn02", "CREDIT", "DONE", stepPayload(t, "t3", 2, "CREDIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "DONE", stepPayload(t, "t3", 3, "COMPENSATE_DEBIT")},
			}},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
		sqltest.Rule{Contains: "SELECT step_no, dcn, payload",
			Columns: []string{"step_no", "dcn", "payload"},
			Rows:    [][]any{{int64(4), "dcn02", stepPayload(t, "t3", 4, "COMPENSATE_CREDIT")}}},
	)
	var published []string
	c := &Coordinator{db: db, publishFn: func(exchange, key string, body []byte) error {
		published = append(published, string(body))
		return nil
	}}
	rc, _ := json.Marshal(contracts.Receipt{TxID: "t3", StepNo: 2, DCN: "dcn02", Status: "DONE"})
	if err := c.handleReceipt(rc); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE tx_log SET status = 'PROCESSING'") != 1 {
		t.Fatal("tx should be reopened to PROCESSING")
	}
	ins := rec.LastExec("INSERT INTO tx_step_log")
	if ins == nil || ins.Args[3] != "COMPENSATE_CREDIT" {
		t.Fatalf("want COMPENSATE_CREDIT inserted for late DONE, got %+v", ins)
	}
	if len(published) == 0 {
		t.Fatal("re-compensation not published")
	}
}

// advance：补偿步骤全部 DONE → COMPENSATED 终态。
func TestAdvanceCompensationCompleteTerminates(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "t4", 1, "DEBIT")},
				{int64(2), "dcn02", "CREDIT", "FAILED", stepPayload(t, "t4", 2, "CREDIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "DONE", stepPayload(t, "t4", 3, "COMPENSATE_DEBIT")},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	if err := c.advance("t4"); err != nil {
		t.Fatal(err)
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMPENSATED" {
		t.Fatalf("want COMPENSATED transition, got %+v", upd)
	}
}

func result1() driver.Result { return oneRow{} }

type oneRow struct{}

func (oneRow) LastInsertId() (int64, error) { return 0, nil }
func (oneRow) RowsAffected() (int64, error) { return 1, nil }
