package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
)

// AccountStore Persistence interface (repo implementation) for account use cases.
type AccountStore interface {
	InsertDemand(ctx context.Context, a domain.DemandAccount) error
	InsertFixed(ctx context.Context, a domain.FixedAccount) error
	SetDemandStatus(ctx context.Context, accountNo string, status domain.AccountStatus) error
}

type AccountService struct {
	store AccountStore
}

func NewAccountService(store AccountStore) *AccountService {
	return &AccountService{store: store}
}

// Open a current account with OpenDemand. When the status is not specified, the default is active; new accounts are forced to be active.
func (s *AccountService) OpenDemand(ctx context.Context, a domain.DemandAccount) error {
	if a.Status == "" {
		a.Status = domain.AccountStatusActive
	}
	if a.Status != domain.AccountStatusActive {
		return fmt.Errorf("account: 新开户必须 active, got %q", a.Status)
	}
	return s.store.InsertDemand(ctx, a)
}

// OpenFixed opens a fixed account (also defaults to active).
func (s *AccountService) OpenFixed(ctx context.Context, a domain.FixedAccount) error {
	if a.Status == "" {
		a.Status = domain.AccountStatusActive
	}
	return s.store.InsertFixed(ctx, a)
}

// Close Account cancellation: active→closed after state machine verification.
func (s *AccountService) Close(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Close(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}

// Freeze: active→frozen.
func (s *AccountService) Freeze(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Freeze(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}

// Unfreeze: frozen→active.
func (s *AccountService) Unfreeze(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Unfreeze(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}
