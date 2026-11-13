//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"bank/core-banking/corebanking/domain"
	"bank/core-banking/corebanking/repo"
	"bank/core-banking/corebanking/service"
	"bank/lib/pg"
)

// seedHoldAccount inserts a demand account + a historical-day balance row
// (100.00 yuan = 10000 cents) and returns the account number. The current
// biz_date is advanced to 2026-07-16 so EnsureBalanceRow/LockLatestBalance
// inherit from the seeded historical row.
func seedHoldAccount(t *testing.T, db *sql.DB, acct string) {
	t.Helper()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)

	db.ExecContext(ctx, "DELETE FROM funds_hold WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", acct)
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", acct)
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: acct, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	// Historical-day balance: 100.00 yuan (stored as NUMERIC(18,2) yuan).
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ($1,'2026-07-15',100.00,100.00,'2011')`, acct)

	// Advance biz_date so LockLatestBalance inherits the historical row.
	var prev string
	if err := db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&prev); err != nil {
		t.Fatalf("read biz_date: %v", err)
	}
	db.ExecContext(ctx, "UPDATE sys_param SET param_value='2026-07-16' WHERE param_key='biz_date'")
	t.Cleanup(func() {
		db.ExecContext(context.Background(), "UPDATE sys_param SET param_value=$1 WHERE param_key='biz_date'", prev)
	})
}

// TestHoldRepo_PlaceDuplicateRelease exercises the full PlaceHold/ReleaseHold
// lifecycle through the real repo + service against a live PG instance.
func TestHoldRepo_PlaceDuplicateRelease(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()

	const acct = "HOLD-A1"
	seedHoldAccount(t, db, acct)

	hr := repo.NewHoldRepo(db)
	hs := service.NewHoldService(db, hr)

	// Place a 30.00-yuan hold (3000 cents). Available 100-0=100 >= 30 -> OK.
	hold1, err := hs.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "IK1", AccountNo: acct,
		Amount: domain.NewMoneyFromCents(3000), Ccy: "CNY", WorkflowID: "WF1",
	})
	if err != nil {
		t.Fatalf("PlaceHold #1 failed: %v", err)
	}
	if hold1.Status != domain.HoldStatusActive {
		t.Errorf("hold1 status=%q want active", hold1.Status)
	}
	if hold1.HoldID == "" {
		t.Error("hold1 ID should not be empty")
	}

	// Duplicate idempotency key -> same hold, no new insert.
	dup, err := hs.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "IK1", AccountNo: acct,
		Amount: domain.NewMoneyFromCents(3000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("duplicate PlaceHold should succeed: %v", err)
	}
	if dup.HoldID != hold1.HoldID {
		t.Errorf("duplicate should return same hold, got %s want %s", dup.HoldID, hold1.HoldID)
	}

	// Place a second 60.00-yuan hold. Available 100-30=70 >= 60 -> OK.
	hold2, err := hs.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "IK2", AccountNo: acct,
		Amount: domain.NewMoneyFromCents(6000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("PlaceHold #2 should succeed: %v", err)
	}

	// Available now 100-30-60=10. A 20.00-yuan hold must be rejected.
	_, err = hs.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "IK3", AccountNo: acct,
		Amount: domain.NewMoneyFromCents(2000), Ccy: "CNY",
	})
	if !errors.Is(err, service.ErrInsufficientAvailableBalance) {
		t.Fatalf("insufficient available should give ErrInsufficientAvailableBalance, got %v", err)
	}

	// Release hold2 -> available returns to 100-30=70.
	released, err := hs.ReleaseHold(ctx, hold2.HoldID, "RK1")
	if err != nil {
		t.Fatalf("ReleaseHold failed: %v", err)
	}
	if released.Status != domain.HoldStatusReleased {
		t.Errorf("released status=%q want released", released.Status)
	}

	// Idempotent re-release: no error, same hold.
	released2, err := hs.ReleaseHold(ctx, hold2.HoldID, "RK1")
	if err != nil {
		t.Fatalf("duplicate release should succeed idempotently: %v", err)
	}
	if released2.Status != domain.HoldStatusReleased {
		t.Errorf("released2 status=%q want released", released2.Status)
	}

	// After release, the 20.00-yuan hold that was rejected should now succeed.
	hold3, err := hs.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "IK3b", AccountNo: acct,
		Amount: domain.NewMoneyFromCents(2000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("should be able to hold again after release: %v", err)
	}
	if hold3.Status != domain.HoldStatusActive {
		t.Errorf("hold3 status=%q want active", hold3.Status)
	}

	// Verify DB row counts: 3 holds total (IK1 active, IK2 released, IK3b active).
	var count int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM funds_hold WHERE account_no=$1", acct).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Errorf("DB holds count=%d want 3", count)
	}
}

// TestHoldRepo_CapturedCannotBeReleased verifies the captured->released
// transition is rejected when the hold has been externally captured.
func TestHoldRepo_CapturedCannotBeReleased(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()

	const acct = "HOLD-C1"
	seedHoldAccount(t, db, acct)

	hr := repo.NewHoldRepo(db)
	hs := service.NewHoldService(db, hr)

	hold, err := hs.PlaceHold(ctx, domain.PlaceHoldInput{
		IdempotencyKey: "CK1", AccountNo: acct,
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate Task-3 capture: externally mark the hold as captured.
	if _, err := db.ExecContext(ctx,
		"UPDATE funds_hold SET status='captured', updated_at=now() WHERE hold_id=$1",
		hold.HoldID); err != nil {
		t.Fatalf("manually set captured failed: %v", err)
	}

	_, err = hs.ReleaseHold(ctx, hold.HoldID, "RK1")
	if !errors.Is(err, domain.ErrHoldCaptured) {
		t.Fatalf("releasing captured hold should give ErrHoldCaptured, got %v", err)
	}
}

// TestHoldRepo_NotFoundErrors verifies sentinel-error mapping for missing holds.
func TestHoldRepo_NotFoundErrors(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	hr := repo.NewHoldRepo(db)

	_, err := hr.GetHoldByIdempotencyKey(ctx, db, "NOPE")
	if !errors.Is(err, service.ErrHoldNotFound) {
		t.Errorf("GetHoldByIdempotencyKey not found should give ErrHoldNotFound, got %v", err)
	}

	// LockHoldByID runs inside a tx (FOR UPDATE).
	pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		_, err := hr.LockHoldByID(ctx, q, "NOPE")
		if !errors.Is(err, service.ErrHoldNotFound) {
			t.Errorf("LockHoldByID not found should give ErrHoldNotFound, got %v", err)
		}
		return nil
	})
}
