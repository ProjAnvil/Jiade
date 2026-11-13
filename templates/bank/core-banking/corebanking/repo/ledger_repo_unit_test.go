package repo

// Unit tests for LedgerRepo write/read paths using the fakesql harness in
// fakesql_test.go (no live PostgreSQL). Live-DB round-trips remain in the
// integration-gated integration_test.go.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"bank/core-banking/corebanking/domain"
	"bank/core-banking/corebanking/service"
	"bank/lib/messaging"
)

func TestLedgerRepo_InsertTxns_GeneratesMissingIDs(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into acct_txn", affected: 1})
	r := NewLedgerRepo(db.open())
	txns := []domain.Txn{
		{BizDate: "2026-07-31", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY", VoucherNo: "V1", TxnStatus: domain.TxnStatusNormal},
		{TxnID: "T-fixed", BizDate: "2026-07-31", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY", VoucherNo: "V1", TxnStatus: domain.TxnStatusNormal},
	}
	if err := r.InsertTxns(context.Background(), db.open(), txns); err != nil {
		t.Fatalf("InsertTxns: %v", err)
	}
	calls := db.callsFor("insert into acct_txn")
	if len(calls) != 2 {
		t.Fatalf("inserts = %d, want 2", len(calls))
	}
	if id := calls[0].args[0].(string); !strings.HasPrefix(id, "T") || len(id) != 17 {
		t.Errorf("generated txn id = %q, want T + 16 hex chars", id)
	}
	if calls[1].args[0] != "T-fixed" {
		t.Errorf("explicit txn id must be kept, got %v", calls[1].args[0])
	}
	if calls[0].args[4] != "100.00" {
		t.Errorf("amount not serialised as cents string: %v", calls[0].args[4])
	}
}

func TestLedgerRepo_InsertTxns_ErrorWrapped(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into acct_txn", err: errors.New("check violation")})
	r := NewLedgerRepo(db.open())
	txns := []domain.Txn{{TxnID: "T1", Amount: domain.NewMoneyFromCents(1), VoucherNo: "V1"}}
	if err := r.InsertTxns(context.Background(), db.open(), txns); err == nil || !strings.Contains(err.Error(), "check violation") {
		t.Fatalf("want wrapped error, got %v", err)
	}
}

func TestLedgerRepo_ApplyBalanceDeltas(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into account_balance", affected: 1})
	r := NewLedgerRepo(db.open())
	deltas := []domain.BalanceDelta{
		{AccountNo: "D1", Delta: domain.NewMoneyFromCents(-5000), SubjectCode: "1001"},
		{AccountNo: "D2", Delta: domain.NewMoneyFromCents(5000), SubjectCode: "1001"},
	}
	if err := r.ApplyBalanceDeltas(context.Background(), db.open(), "2026-07-31", deltas); err != nil {
		t.Fatalf("ApplyBalanceDeltas: %v", err)
	}
	calls := db.callsFor("insert into account_balance")
	if len(calls) != 2 || calls[0].args[2] != "-50.00" {
		t.Errorf("deltas wrong: %+v", calls)
	}
}

func TestLedgerRepo_UpsertGL(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into gl_balance", affected: 1})
	r := NewLedgerRepo(db.open())
	if err := r.UpsertGL(context.Background(), db.open(), domain.GLBalance{
		SubjectCode: "1001", BizDate: "2026-07-31", DCBalance: domain.NewMoneyFromCents(10000), CCBalance: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	}); err != nil {
		t.Fatalf("UpsertGL: %v", err)
	}
	if call := db.lastCallWith("insert into gl_balance"); call == nil || call.args[2] != "100.00" {
		t.Errorf("gl args wrong: %+v", call)
	}

	fail := &fakeDB{}
	fail.add(queryPlan{marker: "insert into gl_balance", err: errors.New("boom")})
	r2 := NewLedgerRepo(fail.open())
	if err := r2.UpsertGL(context.Background(), fail.open(), domain.GLBalance{}); err == nil {
		t.Fatal("upsert error should surface")
	}
}

func TestLedgerRepo_GetGL(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from gl_balance",
		cols:   []string{"subject_code", "biz_date", "dc_balance", "cc_balance", "ccy"},
		rows:   [][]driver.Value{{"1001", "2026-07-31", "1000.00", "900.00", "CNY"}},
	})
	r := NewLedgerRepo(db.open())
	gls, err := r.GetGL(context.Background(), "2026-07-31")
	if err != nil || len(gls) != 1 || gls[0].DCBalance.Cents() != 100000 || gls[0].CCBalance.Cents() != 90000 {
		t.Fatalf("GetGL: %+v %v", gls, err)
	}

	bad := &fakeDB{}
	bad.add(queryPlan{
		marker: "from gl_balance",
		cols:   []string{"subject_code", "biz_date", "dc_balance", "cc_balance", "ccy"},
		rows:   [][]driver.Value{{"1001", "2026-07-31", "NaN", "1.00", "CNY"}},
	})
	r2 := NewLedgerRepo(bad.open())
	if _, err := r2.GetGL(context.Background(), "2026-07-31"); err == nil {
		t.Fatal("bad dc_balance should fail")
	}
}

func TestLedgerRepo_EnsureBalanceRow(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into account_balance", affected: 1})
	db.add(queryPlan{marker: "for update", cols: []string{"balance", "available_balance", "frozen_amount"}, rows: [][]driver.Value{{"1000.00", "800.00", "200.00"}}})
	r := NewLedgerRepo(db.open())
	b, err := r.EnsureBalanceRow(context.Background(), db.open(), "D1", "2026-07-31", "1001")
	if err != nil {
		t.Fatalf("EnsureBalanceRow: %v", err)
	}
	if b.SubjectCode != "1001" || b.Balance.Cents() != 100000 || b.AvailableBalance.Cents() != 80000 {
		t.Errorf("balance wrong: %+v", b)
	}

	missing := &fakeDB{}
	missing.add(queryPlan{marker: "insert into account_balance", affected: 0})
	missing.add(queryPlan{marker: "for update", cols: []string{"balance", "available_balance", "frozen_amount"}})
	r2 := NewLedgerRepo(missing.open())
	if _, err := r2.EnsureBalanceRow(context.Background(), missing.open(), "D1", "2026-07-31", ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing row should surface ErrNoRows, got %v", err)
	}
}

const voucherTxnCols = "txn_id,biz_date,txn_ts,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary,txn_status"

func voucherRow(id string) []driver.Value {
	return []driver.Value{id, "2026-07-31", "2026-07-31 12:00:00", "D1", "D", "100.00", "CNY", "1001", "D2", "", "online", "", "normal"}
}

func TestLedgerRepo_GetAndLockTxnsByVoucher(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from acct_txn", cols: strings.Split(voucherTxnCols, ","), rows: [][]driver.Value{voucherRow("T1")}})
	r := NewLedgerRepo(db.open())
	txns, err := r.GetTxnsByVoucher(context.Background(), db.open(), "V1")
	if err != nil || len(txns) != 1 || txns[0].VoucherNo != "V1" || txns[0].Amount.Cents() != 10000 {
		t.Fatalf("GetTxnsByVoucher: %+v %v", txns, err)
	}

	lockDB := &fakeDB{}
	lockDB.add(queryPlan{marker: "from acct_txn", cols: strings.Split(voucherTxnCols, ","), rows: [][]driver.Value{voucherRow("T1")}})
	r2 := NewLedgerRepo(lockDB.open())
	txns2, err := r2.LockTxnsByVoucher(context.Background(), lockDB.open(), "V1")
	if err != nil || len(txns2) != 1 {
		t.Fatalf("LockTxnsByVoucher: %+v %v", txns2, err)
	}

	bad := &fakeDB{}
	bad.add(queryPlan{marker: "from acct_txn", cols: strings.Split(voucherTxnCols, ","), rows: [][]driver.Value{{"T1", "d", "ts", "D1", "D", "bad", "CNY", "s", "", "", "", "", "normal"}}})
	r3 := NewLedgerRepo(bad.open())
	if _, err := r3.GetTxnsByVoucher(context.Background(), bad.open(), "V1"); err == nil {
		t.Fatal("bad amount should fail the voucher scan")
	}
}

func TestLedgerRepo_HasReversal(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "select exists", cols: []string{"exists"}, rows: [][]driver.Value{{true}}})
	r := NewLedgerRepo(db.open())
	got, err := r.HasReversal(context.Background(), db.open(), "T1")
	if err != nil || !got {
		t.Fatalf("HasReversal = %v, %v", got, err)
	}
}

func TestLedgerRepo_UpdateTxnStatus(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "update acct_txn set txn_status", affected: 2})
	r := NewLedgerRepo(db.open())
	if err := r.UpdateTxnStatus(context.Background(), db.open(), "V1", domain.TxnStatusReversed); err != nil {
		t.Fatalf("UpdateTxnStatus: %v", err)
	}
	if call := db.lastCallWith("update acct_txn"); call == nil || call.args[1] != "reversed" {
		t.Errorf("status not passed: %+v", call)
	}

	zero := &fakeDB{}
	zero.add(queryPlan{marker: "update acct_txn set txn_status", affected: 0})
	r2 := NewLedgerRepo(zero.open())
	err := r2.UpdateTxnStatus(context.Background(), zero.open(), "V1", domain.TxnStatusReversed)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("zero rows must surface sql.ErrNoRows (already reversed), got %v", err)
	}

	fail := &fakeDB{}
	fail.add(queryPlan{marker: "update acct_txn set txn_status", err: errors.New("deadlock")})
	r3 := NewLedgerRepo(fail.open())
	if err := r3.UpdateTxnStatus(context.Background(), fail.open(), "V1", domain.TxnStatusReversed); err == nil || !strings.Contains(err.Error(), "deadlock") {
		t.Fatalf("want wrapped error, got %v", err)
	}
}

func TestLedgerRepo_SetTxnSummary(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "update acct_txn set summary", affected: 1})
	r := NewLedgerRepo(db.open())
	if err := r.SetTxnSummary(context.Background(), db.open(), "V1", "transfer"); err != nil {
		t.Fatalf("SetTxnSummary: %v", err)
	}
	if call := db.lastCallWith("update acct_txn"); call == nil || call.args[1] != "transfer" {
		t.Errorf("summary not passed: %+v", call)
	}
}

func TestLedgerRepo_GetBizDate(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from sys_param", cols: []string{"param_value"}, rows: [][]driver.Value{{"2026-07-31"}}})
	r := NewLedgerRepo(db.open())
	v, err := r.GetBizDate(context.Background())
	if err != nil || v != "2026-07-31" {
		t.Fatalf("GetBizDate = %q, %v", v, err)
	}

	fail := &fakeDB{}
	fail.add(queryPlan{marker: "from sys_param", cols: []string{"param_value"}, err: errors.New("boom")})
	r2 := NewLedgerRepo(fail.open())
	if _, err := r2.GetBizDate(context.Background()); err == nil {
		t.Fatal("missing biz_date should fail")
	}
}

func TestLedgerRepo_HeldTransferIdempotency(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from held_transfer", cols: []string{"voucher_no"}, rows: [][]driver.Value{{"V1"}}})
	r := NewLedgerRepo(db.open())
	v, err := r.GetHeldTransferByKey(context.Background(), db.open(), "idem-1")
	if err != nil || v != "V1" {
		t.Fatalf("GetHeldTransferByKey: %q %v", v, err)
	}

	empty := &fakeDB{}
	empty.add(queryPlan{marker: "from held_transfer", cols: []string{"voucher_no"}})
	r2 := NewLedgerRepo(empty.open())
	if _, err := r2.GetHeldTransferByKey(context.Background(), empty.open(), "nope"); !errors.Is(err, service.ErrHeldTransferNotFound) {
		t.Fatalf("want ErrHeldTransferNotFound, got %v", err)
	}

	ins := &fakeDB{}
	ins.add(queryPlan{marker: "insert into held_transfer", affected: 1})
	r3 := NewLedgerRepo(ins.open())
	if err := r3.InsertHeldTransfer(context.Background(), ins.open(), "idem-1", "V1", "H1"); err != nil {
		t.Fatalf("InsertHeldTransfer: %v", err)
	}
	if call := ins.lastCallWith("insert into held_transfer"); call == nil || call.args[1] != "V1" || call.args[2] != "H1" {
		t.Errorf("held_transfer args wrong: %+v", call)
	}
}

func TestLedgerRepo_VoucherReversalTracking(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "select exists", cols: []string{"exists"}, rows: [][]driver.Value{{true}}})
	db.add(queryPlan{marker: "insert into voucher_reversal", affected: 1})
	r := NewLedgerRepo(db.open())
	ctx := context.Background()

	got, err := r.HasReversalForVoucher(ctx, db.open(), "V1")
	if err != nil || !got {
		t.Fatalf("HasReversalForVoucher = %v, %v", got, err)
	}
	if err := r.InsertReversal(ctx, db.open(), "V1", "V2"); err != nil {
		t.Fatalf("InsertReversal: %v", err)
	}
	if call := db.lastCallWith("insert into voucher_reversal"); call == nil || call.args[0] != "V1" || call.args[1] != "V2" {
		t.Errorf("reversal args wrong: %+v", call)
	}
}

func TestLedgerRepo_AppendOutbox(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into outbox_message", affected: 1})
	r := NewLedgerRepo(db.open())
	env := messaging.Envelope{MessageID: "m-1", MessageType: "core.hold-placed.v1", SchemaVersion: messaging.CurrentSchemaVersion}
	if err := r.AppendOutbox(context.Background(), db.open(), env, "core.hold.placed"); err != nil {
		t.Fatalf("AppendOutbox: %v", err)
	}
	if call := db.lastCallWith("insert into outbox_message"); call == nil || call.args[0] != "m-1" || call.args[3] != "core.hold.placed" {
		t.Errorf("outbox args wrong: %+v", call)
	}

	fail := &fakeDB{}
	fail.add(queryPlan{marker: "insert into outbox_message", err: errors.New("insert failed")})
	r2 := NewLedgerRepo(fail.open())
	err := r2.AppendOutbox(context.Background(), fail.open(), env, "core.hold.placed")
	if err == nil || !strings.Contains(err.Error(), "insert failed") {
		t.Fatalf("want wrapped error, got %v", err)
	}
}
