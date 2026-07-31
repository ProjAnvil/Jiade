package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// fakeHoldStore — in-memory HoldStore for unit tests.
type fakeHoldStore struct {
	balance   domain.Balance
	holds     []domain.Hold
	inserted  []domain.Hold
	statusLog []holdStatusCall
}

type holdStatusCall struct {
	holdID string
	status string
}

func (f *fakeHoldStore) LockLatestBalance(_ context.Context, _ pg.DBTX, _ string) (domain.Balance, error) {
	return f.balance, nil
}

func (f *fakeHoldStore) LockActiveHolds(_ context.Context, _ pg.DBTX, accountNo string) ([]domain.Hold, error) {
	var active []domain.Hold
	for _, h := range f.holds {
		if h.AccountNo == accountNo && h.Status == domain.HoldStatusActive {
			active = append(active, h)
		}
	}
	return active, nil
}

func (f *fakeHoldStore) GetHoldByIdempotencyKey(_ context.Context, _ pg.DBTX, key string) (domain.Hold, error) {
	for _, h := range f.holds {
		if h.IdempotencyKey == key {
			return h, nil
		}
	}
	return domain.Hold{}, ErrHoldNotFound
}

func (f *fakeHoldStore) InsertHold(_ context.Context, _ pg.DBTX, h domain.Hold) error {
	f.inserted = append(f.inserted, h)
	f.holds = append(f.holds, h)
	return nil
}

func (f *fakeHoldStore) LockHoldByID(_ context.Context, _ pg.DBTX, holdID string) (domain.Hold, error) {
	for _, h := range f.holds {
		if h.HoldID == holdID {
			return h, nil
		}
	}
	return domain.Hold{}, ErrHoldNotFound
}

func (f *fakeHoldStore) SetHoldStatus(_ context.Context, _ pg.DBTX, holdID string, status domain.HoldStatus) error {
	f.statusLog = append(f.statusLog, holdStatusCall{holdID, string(status)})
	for i := range f.holds {
		if f.holds[i].HoldID == holdID {
			f.holds[i].Status = status
		}
	}
	return nil
}

// --- Brief Step 1 invariants ---

func TestPlaceHoldRejectsInsufficientAvailableBalance(t *testing.T) {
	store := &fakeHoldStore{
		balance: domain.Balance{Balance: domain.NewMoneyFromCents(10000)}, // 100.00
	}
	svc := NewHoldService(nil, store)
	_, err := svc.PlaceHold(context.Background(), domain.PlaceHoldInput{
		IdempotencyKey: "K1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(15000), // 150.00 > 100.00
		Ccy:    "CNY",
	})
	if !errors.Is(err, ErrInsufficientAvailableBalance) {
		t.Fatalf("可用余额不足应 ErrInsufficientAvailableBalance, got %v", err)
	}
	if len(store.inserted) != 0 {
		t.Errorf("不应插入 hold, got %d", len(store.inserted))
	}
}

func TestDuplicatePlaceHoldReturnsExistingHold(t *testing.T) {
	existing := domain.Hold{
		HoldID: "H-EXIST", IdempotencyKey: "K1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Status: domain.HoldStatusActive,
	}
	store := &fakeHoldStore{
		balance: domain.Balance{Balance: domain.NewMoneyFromCents(10000)},
		holds:   []domain.Hold{existing},
	}
	svc := NewHoldService(nil, store)
	got, err := svc.PlaceHold(context.Background(), domain.PlaceHoldInput{
		IdempotencyKey: "K1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("重复 key 应返回现有 hold: %v", err)
	}
	if got.HoldID != "H-EXIST" {
		t.Errorf("应返回现有 hold H-EXIST, got %s", got.HoldID)
	}
	if len(store.inserted) != 0 {
		t.Errorf("不应插入新 hold, got %d inserts", len(store.inserted))
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	existing := domain.Hold{
		HoldID: "H1", IdempotencyKey: "RK1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Status: domain.HoldStatusReleased,
	}
	store := &fakeHoldStore{holds: []domain.Hold{existing}}
	svc := NewHoldService(nil, store)
	got, err := svc.ReleaseHold(context.Background(), "H1", "RK1")
	if err != nil {
		t.Fatalf("释放已释放 hold 应幂等成功: %v", err)
	}
	if got.Status != domain.HoldStatusReleased {
		t.Errorf("状态=%q, want released", got.Status)
	}
	if len(store.statusLog) != 0 {
		t.Errorf("幂等释放不应调用 SetHoldStatus, got %d calls", len(store.statusLog))
	}
}

func TestCapturedHoldCannotBeReleased(t *testing.T) {
	existing := domain.Hold{
		HoldID: "H1", IdempotencyKey: "RK1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Status: domain.HoldStatusCaptured,
	}
	store := &fakeHoldStore{holds: []domain.Hold{existing}}
	svc := NewHoldService(nil, store)
	_, err := svc.ReleaseHold(context.Background(), "H1", "RK1")
	if !errors.Is(err, domain.ErrHoldCaptured) {
		t.Fatalf("已捕获 hold 释放应 ErrHoldCaptured, got %v", err)
	}
	if len(store.statusLog) != 0 {
		t.Errorf("不应更新状态, got %d calls", len(store.statusLog))
	}
}

// --- Positive + supplementary tests ---

func TestPlaceHold_Success(t *testing.T) {
	store := &fakeHoldStore{
		balance: domain.Balance{Balance: domain.NewMoneyFromCents(10000)},
	}
	svc := NewHoldService(nil, store)
	got, err := svc.PlaceHold(context.Background(), domain.PlaceHoldInput{
		IdempotencyKey: "K1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY", WorkflowID: "WF1",
	})
	if err != nil {
		t.Fatalf("PlaceHold 应成功: %v", err)
	}
	if got.Status != domain.HoldStatusActive {
		t.Errorf("状态=%q, want active", got.Status)
	}
	if got.HoldID == "" || got.IdempotencyKey != "K1" || got.WorkflowID != "WF1" {
		t.Errorf("hold 字段不对: %+v", got)
	}
}

func TestPlaceHold_AvailableReducedByExistingHolds(t *testing.T) {
	store := &fakeHoldStore{
		balance: domain.Balance{Balance: domain.NewMoneyFromCents(10000)}, // 100.00
		holds: []domain.Hold{
			{HoldID: "H0", IdempotencyKey: "K0", AccountNo: "A1", Amount: domain.NewMoneyFromCents(6000), Status: domain.HoldStatusActive},
		},
	}
	svc := NewHoldService(nil, store)
	// 100.00 - 60.00 (active hold) = 40.00 available; 50.00 > 40.00 -> reject
	_, err := svc.PlaceHold(context.Background(), domain.PlaceHoldInput{
		IdempotencyKey: "K1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY",
	})
	if !errors.Is(err, ErrInsufficientAvailableBalance) {
		t.Fatalf("扣减已有 hold 后可用不足应拒绝: got %v", err)
	}
}

func TestPlaceHold_NonPositiveAmount_Rejected(t *testing.T) {
	svc := NewHoldService(nil, &fakeHoldStore{})
	_, err := svc.PlaceHold(context.Background(), domain.PlaceHoldInput{
		IdempotencyKey: "K1", AccountNo: "A1", Amount: domain.NewMoneyFromCents(0), Ccy: "CNY",
	})
	if !errors.Is(err, ErrNonPositiveHoldAmount) {
		t.Fatalf("非正金额应 ErrNonPositiveHoldAmount, got %v", err)
	}
}

func TestReleaseHold_Active_Success(t *testing.T) {
	existing := domain.Hold{
		HoldID: "H1", IdempotencyKey: "RK1", AccountNo: "A1",
		Amount: domain.NewMoneyFromCents(5000), Status: domain.HoldStatusActive,
	}
	store := &fakeHoldStore{holds: []domain.Hold{existing}}
	svc := NewHoldService(nil, store)
	got, err := svc.ReleaseHold(context.Background(), "H1", "RK1")
	if err != nil {
		t.Fatalf("释放 active hold 应成功: %v", err)
	}
	if got.Status != domain.HoldStatusReleased {
		t.Errorf("状态=%q, want released", got.Status)
	}
	if len(store.statusLog) != 1 || store.statusLog[0].status != string(domain.HoldStatusReleased) {
		t.Errorf("应调用 SetHoldStatus=released once, got %v", store.statusLog)
	}
}

func TestReleaseHold_NotFound(t *testing.T) {
	svc := NewHoldService(nil, &fakeHoldStore{})
	_, err := svc.ReleaseHold(context.Background(), "NOPE", "RK1")
	if !errors.Is(err, ErrHoldNotFound) {
		t.Fatalf("不存在应 ErrHoldNotFound, got %v", err)
	}
}
