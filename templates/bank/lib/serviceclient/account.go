package serviceclient

import (
	"context"

	corev1 "bank/gen/bank/core/v1"
)

// Account is the account data needed by bank service compositions.
type Account struct {
	AccountNo             string
	CustomerID            string
	Currency              string
	Status                string
	LedgerBalanceMinor    int64
	AvailableBalanceMinor int64
	OpenBizDate           string
	Branch                string
}

// AccountReader reads an account snapshot without exposing transport details.
type AccountReader interface {
	GetAccount(context.Context, string, string) (Account, error)
}

type accountReader struct {
	client corev1.AccountQueryServiceClient
}

// NewAccountReader adapts the generated account query client to AccountReader.
func NewAccountReader(client corev1.AccountQueryServiceClient) AccountReader {
	return accountReader{client: client}
}

func (r accountReader) GetAccount(ctx context.Context, accountNo, requestID string) (Account, error) {
	snapshot, err := r.client.GetAccount(ctx, &corev1.GetAccountRequest{AccountNo: accountNo, RequestId: requestID})
	if err != nil {
		return Account{}, mapNotFound(err)
	}
	return Account{
		AccountNo:             snapshot.GetAccountNo(),
		CustomerID:            snapshot.GetCustomerId(),
		Currency:              snapshot.GetCurrency(),
		Status:                snapshot.GetStatus(),
		LedgerBalanceMinor:    snapshot.GetLedgerBalanceMinor(),
		AvailableBalanceMinor: snapshot.GetAvailableBalanceMinor(),
		OpenBizDate:           snapshot.GetOpenBizDate(),
		Branch:                snapshot.GetBranch(),
	}, nil
}
