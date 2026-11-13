package repo

// Unit tests for AccountRepo, TxnRepo, and HoldRepo query/scan logic using
// the fakesql harness in fakesql_test.go (no live PostgreSQL). Live-DB
// round-trips remain in the integration-gated *_test.go files.

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"bank/core-banking/corebanking/domain"
	"bank/core-banking/corebanking/service"
)

var testTime = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// --- AccountRepo ---

func TestAccountRepo_InsertDemandAndFixed(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into demand_account", affected: 1})
	db.add(queryPlan{marker: "insert into fixed_account", affected: 1})
	r := NewAccountRepo(db.open())
	ctx := context.Background()

	if err := r.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: "D1", CustID: "C1", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-31", BranchCode: "BR-01", ProductCode: "P1", SubjectCode: "1001",
	}); err != nil {
		t.Fatalf("InsertDemand: %v", err)
	}
	if call := db.lastCallWith("insert into demand_account"); call == nil || call.args[0] != "D1" || call.args[3] != "active" {
		t.Errorf("demand args wrong: %+v", call)
	}

	if err := r.InsertFixed(ctx, domain.FixedAccount{
		AccountNo: "F1", CustID: "C1", Ccy: "CNY", Principal: domain.NewMoneyFromCents(100000),
		Rate: "0.03", TermMonths: 12, StartBizDate: "2026-07-31", MatureDate: "2027-07-31",
		Status: domain.AccountStatusActive, SubjectCode: "1002",
	}); err != nil {
		t.Fatalf("InsertFixed: %v", err)
	}
	if call := db.lastCallWith("insert into fixed_account"); call == nil || call.args[3] != "1000.00" {
		t.Errorf("fixed args wrong: %+v", call)
	}
}

func TestAccountRepo_InsertErrorsWrapped(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into demand_account", err: errors.New("dup key")})
	r := NewAccountRepo(db.open())
	if err := r.InsertDemand(context.Background(), domain.DemandAccount{AccountNo: "D1"}); err == nil || !strings.Contains(err.Error(), "dup key") {
		t.Fatalf("want wrapped error, got %v", err)
	}

	db2 := &fakeDB{}
	db2.add(queryPlan{marker: "insert into fixed_account", err: errors.New("boom")})
	r2 := NewAccountRepo(db2.open())
	if err := r2.InsertFixed(context.Background(), domain.FixedAccount{AccountNo: "F1"}); err == nil {
		t.Fatal("fixed insert error should surface")
	}
}

func TestAccountRepo_SetDemandStatus(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "update demand_account", affected: 1})
	r := NewAccountRepo(db.open())
	if err := r.SetDemandStatus(context.Background(), "D1", domain.AccountStatusFrozen); err != nil {
		t.Fatalf("SetDemandStatus: %v", err)
	}
	if call := db.lastCallWith("update demand_account"); call == nil || call.args[1] != "frozen" {
		t.Errorf("status not passed: %+v", call)
	}

	zero := &fakeDB{}
	zero.add(queryPlan{marker: "update demand_account", affected: 0})
	r2 := NewAccountRepo(zero.open())
	if err := r2.SetDemandStatus(context.Background(), "D-NOPE", domain.AccountStatusClosed); err == nil {
		t.Fatal("zero rows affected should report the account missing")
	}
}

func TestAccountRepo_GetDemand(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from demand_account",
		cols:   []string{"account_no", "cust_id", "ccy", "acct_status", "open_biz_date", "branch_code", "product_code", "subject_code"},
		rows:   [][]driver.Value{{"D1", "C1", "CNY", "active", "2026-07-31", "BR-01", "P1", "1001"}},
	})
	r := NewAccountRepo(db.open())
	a, err := r.GetDemand(context.Background(), "D1")
	if err != nil || a.AccountNo != "D1" || a.Status != domain.AccountStatusActive {
		t.Fatalf("GetDemand: %+v %v", a, err)
	}

	empty := &fakeDB{}
	empty.add(queryPlan{marker: "from demand_account", cols: []string{"account_no"}})
	r2 := NewAccountRepo(empty.open())
	if _, err := r2.GetDemand(context.Background(), "D-NOPE"); err == nil {
		t.Fatal("missing demand account should fail")
	}
}

func TestAccountRepo_GetFixed(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from fixed_account",
		cols:   []string{"account_no", "cust_id", "ccy", "principal", "rate", "term_months", "start_biz_date", "mature_date", "acct_status", "subject_code"},
		rows:   [][]driver.Value{{"F1", "C1", "CNY", "10000.00", "0.030000", int64(12), "2026-07-31", "2027-07-31", "active", "1002"}},
	})
	r := NewAccountRepo(db.open())
	a, err := r.GetFixed(context.Background(), "F1")
	if err != nil || a.Principal.Cents() != 1000000 || a.TermMonths != 12 || a.Rate != "0.030000" {
		t.Fatalf("GetFixed: %+v %v", a, err)
	}

	bad := &fakeDB{}
	bad.add(queryPlan{
		marker: "from fixed_account",
		cols:   []string{"account_no", "cust_id", "ccy", "principal", "rate", "term_months", "start_biz_date", "mature_date", "acct_status", "subject_code"},
		rows:   [][]driver.Value{{"F1", "C1", "CNY", "NaN", "r", int64(1), "d1", "d2", "active", "s"}},
	})
	r2 := NewAccountRepo(bad.open())
	if _, err := r2.GetFixed(context.Background(), "F1"); err == nil {
		t.Fatal("bad principal should fail the scan")
	}
}

// --- TxnRepo ---

const listTxnCols = "txn_id,biz_date,txn_ts,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary"

func TestTxnRepo_ListTxns(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from acct_txn",
		cols:   strings.Split(listTxnCols, ","),
		rows:   [][]driver.Value{{"T1", "2026-07-31", "2026-07-31 12:00:00", "D1", "D", "100.00", "CNY", "1001", "D2", "", "online", "deposit"}},
	})
	r := NewTxnRepo(db.open())
	list, err := r.ListTxns(context.Background(), "D1", "2026-07-01", "2026-07-31")
	if err != nil {
		t.Fatalf("ListTxns: %v", err)
	}
	if len(list) != 1 || list[0].TxnID != "T1" || list[0].Amount.Cents() != 10000 {
		t.Fatalf("txn mapping wrong: %+v", list)
	}
	if call := db.lastCallWith("from acct_txn"); call == nil || len(call.args) != 3 || call.args[0] != "D1" {
		t.Errorf("filters not passed through: %+v", call)
	}

	bad := &fakeDB{}
	bad.add(queryPlan{marker: "from acct_txn", cols: strings.Split(listTxnCols, ","), rows: [][]driver.Value{{"T1", "d", "ts", "D1", "D", "bad", "CNY", "s", "", "", "", ""}}})
	r2 := NewTxnRepo(bad.open())
	if _, err := r2.ListTxns(context.Background(), "", "", ""); err == nil {
		t.Fatal("bad amount should fail")
	}
}

func TestTxnRepo_GetLatestBalance(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{
		marker: "from account_balance",
		cols:   []string{"account_no", "biz_date", "balance", "available_balance", "frozen_amount", "subject_code"},
		rows:   [][]driver.Value{{"D1", "2026-07-31", "1000.00", "800.00", "200.00", "1001"}},
	})
	r := NewTxnRepo(db.open())
	b, err := r.GetLatestBalance(context.Background(), "D1")
	if err != nil || b.Balance.Cents() != 100000 || b.AvailableBalance.Cents() != 80000 || b.FrozenAmount.Cents() != 20000 {
		t.Fatalf("balance mapping wrong: %+v %v", b, err)
	}

	empty := &fakeDB{}
	empty.add(queryPlan{marker: "from account_balance", cols: []string{"account_no"}})
	r2 := NewTxnRepo(empty.open())
	if _, err := r2.GetLatestBalance(context.Background(), "D-NOPE"); err == nil {
		t.Fatal("missing balance row should fail")
	}
}

// --- HoldRepo ---

const holdColsList = "hold_id,idempotency_key,account_no,amount,ccy,workflow_id,status,expires_at,created_at,updated_at"

func holdRow(id string) []driver.Value {
	return []driver.Value{id, "idem-1", "D1", "50.00", "CNY", "wf-1", "active", testTime, testTime, testTime}
}

func TestHoldRepo_LockLatestBalance(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from sys_param", cols: []string{"param_value"}, rows: [][]driver.Value{{"2026-07-31"}}})
	db.add(queryPlan{marker: "insert into account_balance", affected: 1})
	db.add(queryPlan{marker: "for update", cols: []string{"balance", "available_balance", "frozen_amount"}, rows: [][]driver.Value{{"1000.00", "800.00", "200.00"}}})
	r := NewHoldRepo(db.open())
	b, err := r.LockLatestBalance(context.Background(), db.open(), "D1")
	if err != nil {
		t.Fatalf("LockLatestBalance: %v", err)
	}
	if b.BizDate != "2026-07-31" || b.Balance.Cents() != 100000 || b.AvailableBalance.Cents() != 80000 || b.FrozenAmount.Cents() != 20000 {
		t.Errorf("balance wrong: %+v", b)
	}
}

func TestHoldRepo_LockLatestBalance_NoBizDate(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from sys_param", cols: []string{"param_value"}, rows: [][]driver.Value{{""}}})
	r := NewHoldRepo(db.open())
	if _, err := r.LockLatestBalance(context.Background(), db.open(), "D1"); err == nil {
		t.Fatal("empty biz_date should fail")
	}

	missing := &fakeDB{}
	missing.add(queryPlan{marker: "from sys_param", cols: []string{"param_value"}})
	r2 := NewHoldRepo(missing.open())
	if _, err := r2.LockLatestBalance(context.Background(), missing.open(), "D1"); err == nil {
		t.Fatal("missing biz_date row should fail")
	}
}

func TestHoldRepo_LockLatestBalance_MissingBalanceRow(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from sys_param", cols: []string{"param_value"}, rows: [][]driver.Value{{"2026-07-31"}}})
	db.add(queryPlan{marker: "insert into account_balance", affected: 0})
	db.add(queryPlan{marker: "for update", cols: []string{"balance", "available_balance", "frozen_amount"}})
	r := NewHoldRepo(db.open())
	_, err := r.LockLatestBalance(context.Background(), db.open(), "D-EMPTY")
	if !errors.Is(err, service.ErrHoldNotFound) {
		t.Fatalf("want ErrHoldNotFound, got %v", err)
	}
}

func TestHoldRepo_LockActiveHolds(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from funds_hold", cols: strings.Split(holdColsList, ","), rows: [][]driver.Value{
		holdRow("H1"),
		{"H2", "idem-2", "D1", "10.00", "CNY", "wf-2", "active", nil, testTime, testTime},
	}})
	r := NewHoldRepo(db.open())
	holds, err := r.LockActiveHolds(context.Background(), db.open(), "D1")
	if err != nil {
		t.Fatalf("LockActiveHolds: %v", err)
	}
	if len(holds) != 2 || holds[0].Amount.Cents() != 5000 || holds[1].Amount.Cents() != 1000 {
		t.Fatalf("holds mapping wrong: %+v", holds)
	}
	if holds[0].ExpiresAt.IsZero() || !holds[1].ExpiresAt.IsZero() {
		t.Error("expires_at NULL handling wrong")
	}
}

func TestHoldRepo_GetHoldByIdempotencyKey(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from funds_hold", cols: strings.Split(holdColsList, ","), rows: [][]driver.Value{holdRow("H1")}})
	r := NewHoldRepo(db.open())
	h, err := r.GetHoldByIdempotencyKey(context.Background(), db.open(), "idem-1")
	if err != nil || h.HoldID != "H1" || h.Status != domain.HoldStatusActive {
		t.Fatalf("GetHoldByIdempotencyKey: %+v %v", h, err)
	}

	empty := &fakeDB{}
	empty.add(queryPlan{marker: "from funds_hold", cols: strings.Split(holdColsList, ",")})
	r2 := NewHoldRepo(empty.open())
	if _, err := r2.GetHoldByIdempotencyKey(context.Background(), empty.open(), "nope"); !errors.Is(err, service.ErrHoldNotFound) {
		t.Fatalf("want ErrHoldNotFound, got %v", err)
	}
}

func TestHoldRepo_InsertAndSetStatus(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "insert into funds_hold", affected: 1})
	db.add(queryPlan{marker: "update funds_hold", affected: 1})
	r := NewHoldRepo(db.open())
	ctx := context.Background()

	h := domain.Hold{HoldID: "H1", IdempotencyKey: "idem-1", AccountNo: "D1", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY", WorkflowID: "wf-1", Status: domain.HoldStatusActive, ExpiresAt: testTime}
	if err := r.InsertHold(ctx, db.open(), h); err != nil {
		t.Fatalf("InsertHold: %v", err)
	}
	if call := db.lastCallWith("insert into funds_hold"); call == nil || call.args[3] != "50.00" || call.args[7] != testTime {
		t.Errorf("hold args wrong: %+v", call)
	}

	if err := r.SetHoldStatus(ctx, db.open(), "H1", domain.HoldStatusReleased); err != nil {
		t.Fatalf("SetHoldStatus: %v", err)
	}
	if call := db.lastCallWith("update funds_hold"); call == nil || call.args[1] != "released" {
		t.Errorf("status not passed: %+v", call)
	}
}

func TestHoldRepo_LockHoldByID(t *testing.T) {
	db := &fakeDB{}
	db.add(queryPlan{marker: "from funds_hold", cols: strings.Split(holdColsList, ","), rows: [][]driver.Value{holdRow("H1")}})
	r := NewHoldRepo(db.open())
	h, err := r.LockHoldByID(context.Background(), db.open(), "H1")
	if err != nil || h.HoldID != "H1" {
		t.Fatalf("LockHoldByID: %+v %v", h, err)
	}

	empty := &fakeDB{}
	empty.add(queryPlan{marker: "from funds_hold", cols: strings.Split(holdColsList, ",")})
	r2 := NewHoldRepo(empty.open())
	if _, err := r2.LockHoldByID(context.Background(), empty.open(), "H-NOPE"); !errors.Is(err, service.ErrHoldNotFound) {
		t.Fatalf("want ErrHoldNotFound, got %v", err)
	}

	bad := &fakeDB{}
	row := holdRow("H1")
	row[3] = "NaN"
	bad.add(queryPlan{marker: "from funds_hold", cols: strings.Split(holdColsList, ","), rows: [][]driver.Value{row}})
	r3 := NewHoldRepo(bad.open())
	if _, err := r3.LockHoldByID(context.Background(), bad.open(), "H1"); err == nil {
		t.Fatal("bad amount should fail the hold scan")
	}
}
