package domain

import "fmt"

// AccountStatus 账户状态。
type AccountStatus string

const (
	AccountStatusActive AccountStatus = "active"
	AccountStatusClosed AccountStatus = "closed"
	AccountStatusFrozen AccountStatus = "frozen"
)

// 账户状态机（合法迁移）：
//
//	active --Close--> closed（终态）
//	active --Freeze--> frozen
//	frozen --Unfreeze--> active

// Close 销户：仅 active → closed。
func Close(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusActive {
		return AccountStatusClosed, nil
	}
	return from, fmt.Errorf("account: 只有 active 可销户，当前 %q", from)
}

// Freeze 冻结：仅 active → frozen。
func Freeze(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusActive {
		return AccountStatusFrozen, nil
	}
	return from, fmt.Errorf("account: 只有 active 可冻结，当前 %q", from)
}

// Unfreeze 解冻：仅 frozen → active。
func Unfreeze(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusFrozen {
		return AccountStatusActive, nil
	}
	return from, fmt.Errorf("account: 只有 frozen 可解冻，当前 %q", from)
}

// DemandAccount 活期账户（对应 demand_account 表）。
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

// FixedAccount 定期账户（对应 fixed_account 表）。Principal 为金额（分），
// Rate 为 NUMERIC(10,6) 字符串（禁 float 运算，透传）。
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
