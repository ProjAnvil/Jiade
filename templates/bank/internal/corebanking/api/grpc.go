package api

import (
	"context"
	"database/sql"
	"errors"

	corev1 "bank/gen/bank/core/v1"
	"bank/internal/corebanking/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AccountQueryBalanceReader supplies the latest ledger balance for an account.
type AccountQueryBalanceReader interface {
	GetLatestBalance(context.Context, string) (domain.Balance, error)
}

// AccountQueryServer exposes account snapshots to internal callers.
type AccountQueryServer struct {
	corev1.UnimplementedAccountQueryServiceServer
	accounts AccountReader
	balances AccountQueryBalanceReader
}

// NewAccountQueryServer creates the internal account query adapter.
func NewAccountQueryServer(accounts AccountReader, balances AccountQueryBalanceReader) *AccountQueryServer {
	return &AccountQueryServer{accounts: accounts, balances: balances}
}

// GetAccount maps an account and its latest ledger balance to a snapshot.
// Available balance intentionally matches the ledger balance until funds holds
// are implemented by the payment Saga.
func (s *AccountQueryServer) GetAccount(ctx context.Context, req *corev1.GetAccountRequest) (*corev1.AccountSnapshot, error) {
	accountNo := req.GetAccountNo()
	account, err := s.getAccount(ctx, accountNo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "account not found")
		}
		return nil, status.Error(codes.Internal, "get account")
	}
	balance, err := s.balances.GetLatestBalance(ctx, accountNo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "account balance not found")
		}
		return nil, status.Error(codes.Internal, "get account balance")
	}
	return &corev1.AccountSnapshot{
		AccountNo:             account.accountNo,
		CustomerId:            account.customerID,
		Currency:              account.currency,
		Status:                account.status,
		LedgerBalanceMinor:    balance.Balance.Cents(),
		AvailableBalanceMinor: balance.Balance.Cents(),
		OpenBizDate:           account.openBizDate,
		Branch:                account.branch,
	}, nil
}

type accountSnapshotSource struct {
	accountNo   string
	customerID  string
	currency    string
	status      string
	openBizDate string
	branch      string
}

func (s *AccountQueryServer) getAccount(ctx context.Context, accountNo string) (accountSnapshotSource, error) {
	if demand, err := s.accounts.GetDemand(ctx, accountNo); err == nil {
		return accountSnapshotSource{accountNo: demand.AccountNo, customerID: demand.CustID, currency: demand.Ccy, status: string(demand.Status), openBizDate: demand.OpenBizDate, branch: demand.BranchCode}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return accountSnapshotSource{}, err
	}
	fixed, err := s.accounts.GetFixed(ctx, accountNo)
	if err != nil {
		return accountSnapshotSource{}, err
	}
	return accountSnapshotSource{accountNo: fixed.AccountNo, customerID: fixed.CustID, currency: fixed.Ccy, status: string(fixed.Status), openBizDate: fixed.StartBizDate}, nil
}
