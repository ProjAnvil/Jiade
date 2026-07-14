package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
)

// AccountStore 账户用例的持久化接口（repo 实现）。
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

// OpenDemand 开活期账户。未指定状态时默认 active；强制新开户为 active。
func (s *AccountService) OpenDemand(ctx context.Context, a domain.DemandAccount) error {
	if a.Status == "" {
		a.Status = domain.AccountStatusActive
	}
	if a.Status != domain.AccountStatusActive {
		return fmt.Errorf("account: 新开户必须 active, got %q", a.Status)
	}
	return s.store.InsertDemand(ctx, a)
}

// OpenFixed 开定期账户（同样默认 active）。
func (s *AccountService) OpenFixed(ctx context.Context, a domain.FixedAccount) error {
	if a.Status == "" {
		a.Status = domain.AccountStatusActive
	}
	return s.store.InsertFixed(ctx, a)
}

// Close 销户：经状态机校验 active→closed。
func (s *AccountService) Close(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Close(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}

// Freeze 冻结：active→frozen。
func (s *AccountService) Freeze(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Freeze(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}

// Unfreeze 解冻：frozen→active。
func (s *AccountService) Unfreeze(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Unfreeze(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}
