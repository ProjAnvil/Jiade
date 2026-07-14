package service

import (
	"context"

	"bank/internal/corebanking/domain"
)

// TxnStore 流水/余额查询接口（只读，repo 实现）。
type TxnStore interface {
	ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error)
	GetLatestBalance(ctx context.Context, accountNo string) (domain.Balance, error)
}

type TxnService struct {
	store TxnStore
}

func NewTxnService(store TxnStore) *TxnService {
	return &TxnService{store: store}
}

// ListTxns 查流水（from/to 为 YYYY-MM-DD，空表示不限）。
func (s *TxnService) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	return s.store.ListTxns(ctx, accountNo, from, to)
}

// GetBalance 取最新 biz_date 的账户余额。
func (s *TxnService) GetBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	return s.store.GetLatestBalance(ctx, accountNo)
}
