// Package domain is a pure domain model of the loan service, with zero DB/framework dependencies (innermost layer).
// The amount field uses Money (int64 points); rate/min_rate/max_rate is a NUMERIC(10,6) ratio (not an amount), and the text is stored directly.
package domain

// LoanProduct corresponds to the loan_product table.
type LoanProduct struct {
	ProductCode string
	ProductName string
	LoanType    string
	RateType    string
	MinRate     string // NUMERIC(10,6) text (ratio, not amount)
	MaxRate     string
	MaxTerm     int
	MaxAmount   Money
	Status      string
}

// LoanAccount corresponds to the loan_account table.
type LoanAccount struct {
	LoanNo        string
	CustID        string
	ProductCode   string
	Ccy           string
	Principal     Money
	Balance       Money
	Rate          string // NUMERIC(10,6) text (ratio, not amount)
	StartBizDate  string
	MatureDate    string
	TermMonths    int
	Status        string
	GuaranteeType string
	BranchCode    string
}

// LoanDisbursement corresponds to the loan_disbursement table.
type LoanDisbursement struct {
	DisbID    string
	BizDate   string
	LoanNo    string
	Amount    Money
	ToAccount string
}

// LoanRepay corresponds to the loan_repay table.
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

// LoanOverdue corresponds to the loan_overdue table.
type LoanOverdue struct {
	OverdueID     string
	BizDate       string
	LoanNo        string
	OverdueDays   int
	OverdueClass  string
	OverdueAmount Money
}

// LoanBalance corresponds to the loan_balance table (full daily snapshot).
type LoanBalance struct {
	LoanNo             string
	BizDate            string
	PrincipalBalance   Money
	InterestReceivable Money
}

// LoanProfile is the IOU file aggregated by the loan and customer services.
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
