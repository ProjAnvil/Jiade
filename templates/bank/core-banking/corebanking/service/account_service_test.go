package service

import (
	"context"
	"testing"

	"bank/core-banking/corebanking/domain"
)

func TestOpenDemand_RejectsNonActive(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	err := svc.OpenDemand(context.Background(), domain.DemandAccount{
		AccountNo: "D1", Status: domain.AccountStatusFrozen,
	})
	if err == nil {
		t.Error("non-active account opening should error")
	}
	if store.inserted != 0 {
		t.Error("non-active should not be persisted")
	}
}

func TestOpenDemand_DefaultsActive(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	if err := svc.OpenDemand(context.Background(), domain.DemandAccount{AccountNo: "D1"}); err != nil {
		t.Fatalf("default active account opening should succeed: %v", err)
	}
	if store.inserted != 1 || store.last.Status != domain.AccountStatusActive {
		t.Errorf("should persist active account, inserted=%d last=%v", store.inserted, store.last.Status)
	}
}

func TestClose_EnforcesStateMachine(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	// Frozen cannot cancel the account directly: the state machine should reject it
	if err := svc.Close(context.Background(), "D1", domain.AccountStatusFrozen); err == nil {
		t.Error("frozen account should not be closable directly")
	}
	if store.lastStatus == domain.AccountStatusClosed {
		t.Error("invalid transition should not be persisted")
	}
	if err := svc.Close(context.Background(), "D1", domain.AccountStatusActive); err != nil {
		t.Fatalf("closing active account should succeed: %v", err)
	}
	if store.lastStatus != domain.AccountStatusClosed {
		t.Errorf("should be set to closed, got %q", store.lastStatus)
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
