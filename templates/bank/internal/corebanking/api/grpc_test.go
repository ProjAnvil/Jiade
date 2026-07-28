package api

import (
	"context"
	"database/sql"
	"testing"

	corev1 "bank/gen/bank/core/v1"
	"bank/internal/corebanking/domain"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type accountQueryStore struct {
	demand domain.DemandAccount
	fixed  domain.FixedAccount
}

func (s accountQueryStore) GetDemand(context.Context, string) (domain.DemandAccount, error) {
	if s.demand.AccountNo == "" {
		return domain.DemandAccount{}, sql.ErrNoRows
	}
	return s.demand, nil
}

func (s accountQueryStore) GetFixed(context.Context, string) (domain.FixedAccount, error) {
	if s.fixed.AccountNo == "" {
		return domain.FixedAccount{}, sql.ErrNoRows
	}
	return s.fixed, nil
}

type balanceQueryStore struct {
	balance domain.Balance
	err     error
}

func (s balanceQueryStore) GetLatestBalance(context.Context, string) (domain.Balance, error) {
	return s.balance, s.err
}

func TestAccountQueryServerMapsDemandAccountAndLedgerBalance(t *testing.T) {
	server := NewAccountQueryServer(
		accountQueryStore{demand: domain.DemandAccount{AccountNo: "D-7", CustID: "C-42", Ccy: "CNY", Status: domain.AccountStatusActive}},
		balanceQueryStore{balance: domain.Balance{Balance: domain.NewMoneyFromCents(1250)}},
	)

	got, err := server.GetAccount(context.Background(), &corev1.GetAccountRequest{AccountNo: "D-7"})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got.AccountNo != "D-7" || got.CustomerId != "C-42" || got.Currency != "CNY" || got.Status != "active" || got.LedgerBalanceMinor != 1250 || got.AvailableBalanceMinor != 1250 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestAccountQueryServerMapsFixedAccountAndLedgerBalance(t *testing.T) {
	server := NewAccountQueryServer(
		accountQueryStore{fixed: domain.FixedAccount{AccountNo: "F-7", CustID: "C-42", Ccy: "USD", Status: domain.AccountStatusFrozen}},
		balanceQueryStore{balance: domain.Balance{Balance: domain.NewMoneyFromCents(-50)}},
	)

	got, err := server.GetAccount(context.Background(), &corev1.GetAccountRequest{AccountNo: "F-7"})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got.AccountNo != "F-7" || got.CustomerId != "C-42" || got.Currency != "USD" || got.Status != "frozen" || got.LedgerBalanceMinor != -50 || got.AvailableBalanceMinor != -50 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestAccountQueryServerMapsMissingAccountToNotFound(t *testing.T) {
	server := NewAccountQueryServer(accountQueryStore{}, balanceQueryStore{})

	_, err := server.GetAccount(context.Background(), &corev1.GetAccountRequest{AccountNo: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), codes.NotFound, err)
	}
}
