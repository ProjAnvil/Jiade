// Package domain 是 loan 服务的纯领域模型，零 DB/框架依赖（最内层）。
// 金额字段用 Money（int64 分）；rate/min_rate/max_rate 是 NUMERIC(10,6) 比率（非金额），文本直存。
package domain

// LoanProduct 对应 loan_product 表。
type LoanProduct struct {
	ProductCode string
	ProductName string
	LoanType    string
	RateType    string
	MinRate     string // NUMERIC(10,6) 文本（比率，非金额）
	MaxRate     string
	MaxTerm     int
	MaxAmount   Money
	Status      string
}

// LoanAccount 对应 loan_account 表。
type LoanAccount struct {
	LoanNo        string
	CustID        string
	ProductCode   string
	Ccy           string
	Principal     Money
	Balance       Money
	Rate          string // NUMERIC(10,6) 文本（比率，非金额）
	StartBizDate  string
	MatureDate    string
	TermMonths    int
	Status        string
	GuaranteeType string
	BranchCode    string
}

// LoanDisbursement 对应 loan_disbursement 表。
type LoanDisbursement struct {
	DisbID    string
	BizDate   string
	LoanNo    string
	Amount    Money
	ToAccount string
}

// LoanRepay 对应 loan_repay 表。
type LoanRepay struct {
	RepayID       string
	BizDate       string
	LoanNo        string
	DueDate       string
	PrincipalAmt  Money
	InterestAmt   Money
	PaidPrincipal Money
	PaidInterest  Money
	Status        string
}

// LoanOverdue 对应 loan_overdue 表。
type LoanOverdue struct {
	OverdueID     string
	BizDate       string
	LoanNo        string
	OverdueDays   int
	OverdueClass  string
	OverdueAmount Money
}

// LoanBalance 对应 loan_balance 表（逐日全量快照）。
type LoanBalance struct {
	LoanNo             string
	BizDate            string
	PrincipalBalance   Money
	InterestReceivable Money
}

// LoanProfile 是联邦查询结果（loan_account JOIN ext_cust_db_cust_info）。
type LoanProfile struct {
	LoanNo    string
	CustID    string
	Principal Money
	Balance   Money
	Rate      string
	Status    string
	CustName  string
	CustType  string
}
