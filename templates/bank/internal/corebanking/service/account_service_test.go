package service

import (
	"context"
	"testing"

	"bank/internal/corebanking/domain"
)

func TestOpenDemand_RejectsNonActive(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	err := svc.OpenDemand(context.Background(), domain.DemandAccount{
		AccountNo: "D1", Status: domain.AccountStatusFrozen,
	})
	if err == nil {
		t.Error("非 active 开户应报错")
	}
	if store.inserted != 0 {
		t.Error("非 active 不应落库")
	}
}

func TestOpenDemand_DefaultsActive(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	if err := svc.OpenDemand(context.Background(), domain.DemandAccount{AccountNo: "D1"}); err != nil {
		t.Fatalf("默认 active 开户应成功: %v", err)
	}
	if store.inserted != 1 || store.last.Status != domain.AccountStatusActive {
		t.Errorf("应落库 active 账户, inserted=%d last=%v", store.inserted, store.last.Status)
	}
}

func TestClose_EnforcesStateMachine(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	// frozen 不能直接销户：状态机应拒绝
	if err := svc.Close(context.Background(), "D1", domain.AccountStatusFrozen); err == nil {
		t.Error("frozen 状态不应能直接销户")
	}
	if store.lastStatus == domain.AccountStatusClosed {
		t.Error("非法迁移不应落库")
	}
	if err := svc.Close(context.Background(), "D1", domain.AccountStatusActive); err != nil {
		t.Fatalf("active 销户应成功: %v", err)
	}
	if store.lastStatus != domain.AccountStatusClosed {
		t.Errorf("应置为 closed, got %q", store.lastStatus)
	}
}

type recordingAccountStore struct {
	inserted   int
	last       domain.DemandAccount
	lastStatus domain.AccountStatus
}

func (r *recordingAccountStore) InsertDemand(_ context.Context, a domain.DemandAccount) error {
	r.inserted++
	r.last = a
	return nil
}
func (r *recordingAccountStore) InsertFixed(_ context.Context, _ domain.FixedAccount) error {
	return nil
}
func (r *recordingAccountStore) SetDemandStatus(_ context.Context, _ string, s domain.AccountStatus) error {
	r.lastStatus = s
	return nil
}
