package domain

import "fmt"

// AccountStatus Account status.
type AccountStatus string

const (
	AccountStatusActive AccountStatus = "active"
	AccountStatusClosed AccountStatus = "closed"
	AccountStatusFrozen AccountStatus = "frozen"
)

// Account state machine (legal migration):
//
//active --Close--> closed (final state)
//	active --Freeze--> frozen
//	frozen --Unfreeze--> active

// Close account cancellation: only active → closed.
func Close(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusActive {
		return AccountStatusClosed, nil
	}
	return from, fmt.Errorf("account: only active can be closed, current %q", from)
}

// Freeze: only active → frozen.
func Freeze(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusActive {
		return AccountStatusFrozen, nil
	}
	return from, fmt.Errorf("account: only active can be frozen, current %q", from)
}

// Unfreeze: only frozen → active.
func Unfreeze(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusFrozen {
		return AccountStatusActive, nil
	}
	return from, fmt.Errorf("account: only frozen can be unfrozen, current %q", from)
}

// DemandAccount current account (corresponds to demand_account table).
type DemandAccount struct {
	AccountNo   string
	CustID      string
	Ccy         string
	Status      AccountStatus
	OpenBizDate string
	BranchCode  string
	ProductCode string
	SubjectCode string
}

// FixedAccount fixed account (corresponding to fixed_account table). Principal is the amount (cents),
// Rate is a NUMERIC(10,6) string (float operation is prohibited and transparent transmission is prohibited).
type FixedAccount struct {
	AccountNo    string
	CustID       string
	Ccy          string
	Principal    Money
	Rate         string
	TermMonths   int
	StartBizDate string
	MatureDate   string
	Status       AccountStatus
	SubjectCode  string
}
