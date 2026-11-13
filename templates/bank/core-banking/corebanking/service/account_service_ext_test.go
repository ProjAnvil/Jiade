package service

// Tests for the account lifecycle transitions not covered by
// account_service_test.go (OpenFixed default, Freeze/Unfreeze transitions).

import (
	"context"
	"testing"

	"bank/core-banking/corebanking/domain"
)

// recordingAccountStoreExt tracks fixed inserts and every status write; the
// shared recordingAccountStore stub in account_service_test.go does not record
// those.
type recordingAccountStoreExt struct {
	fixed      int
	lastFixed  domain.FixedAccount
	statuses   []domain.AccountStatus
}

func (r *recordingAccountStoreExt) InsertDemand(context.Context, domain.DemandAccount) error {
	return nil
}
func (r *recordingAccountStoreExt) InsertFixed(_ context.Context, a domain.FixedAccount) error {
	r.fixed++
	r.lastFixed = a
	return nil
}
func (r *recordingAccountStoreExt) SetDemandStatus(_ context.Context, _ string, s domain.AccountStatus) error {
	r.statuses = append(r.statuses, s)
	return nil
}

func TestOpenFixed_DefaultsActive(t *testing.T) {
	store := &recordingAccountStoreExt{}
	svc := NewAccountService(store)
	if err := svc.OpenFixed(context.Background(), domain.FixedAccount{AccountNo: "F1"}); err != nil {
		t.Fatalf("OpenFixed: %v", err)
	}
	if store.fixed != 1 || store.lastFixed.Status != domain.AccountStatusActive {
		t.Errorf("fixed open must default to active: %+v", store.lastFixed)
	}

	// An explicit status must be kept as-is.
	if err := svc.OpenFixed(context.Background(), domain.FixedAccount{AccountNo: "F2", Status: domain.AccountStatusActive}); err != nil {
		t.Fatalf("OpenFixed: %v", err)
	}
	if store.lastFixed.Status != domain.AccountStatusActive {
		t.Errorf("explicit status must be kept: %+v", store.lastFixed)
	}
}

func TestFreeze_Unfreeze_Transitions(t *testing.T) {
	store := &recordingAccountStoreExt{}
	svc := NewAccountService(store)
	ctx := context.Background()

	// active → frozen → active.
	if err := svc.Freeze(ctx, "D1", domain.AccountStatusActive); err != nil {
		t.Fatalf("Freeze(active): %v", err)
	}
	if last := store.statuses[len(store.statuses)-1]; last != domain.AccountStatusFrozen {
		t.Errorf("Freeze must write frozen, got %q", last)
	}
	if err := svc.Unfreeze(ctx, "D1", domain.AccountStatusFrozen); err != nil {
		t.Fatalf("Unfreeze(frozen): %v", err)
	}
	if last := store.statuses[len(store.statuses)-1]; last != domain.AccountStatusActive {
		t.Errorf("Unfreeze must write active, got %q", last)
	}

	// Invalid transitions must be rejected by the domain state machine
	// before any store write.
	before := len(store.statuses)
	if err := svc.Unfreeze(ctx, "D1", domain.AccountStatusActive); err == nil {
		t.Error("Unfreeze(active) must be rejected")
	}
	if len(store.statuses) != before {
		t.Error("invalid transition must not reach the store")
	}
}
