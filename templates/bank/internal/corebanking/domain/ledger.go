package domain

// DCFlag Debit and credit flag (core of double-entry accounting).
type DCFlag string

const (
	DCDebit  DCFlag = "借" // Increase in assets and expenses
	DCCredit DCFlag = "贷" // Increase in liabilities and income
)

// Subject accounting account (corresponding to chart_of_acct table).
type Subject struct {
	Code       string
	Name       string
	DCAttr     DCFlag // Account loan attributes
	Level      int
	ParentCode string
	Status     string
}

// Entry A double-entry accounting entry: which account, debit or credit, how much, and into which account.
type Entry struct {
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	SubjectCode string
}

// GLBalance General ledger balance (corresponding to gl_balance table).
type GLBalance struct {
	SubjectCode string
	BizDate     string
	DCBalance   Money // debit balance
	CCBalance   Money // credit balance
	Ccy         string
}

// BalanceDelta is the balance increment of an account on a certain biz_date (credit is positive, debit is negative).
// When posting, it is calculated by service and handed over to repo for accumulation.
type BalanceDelta struct {
	AccountNo   string
	Delta       Money
	SubjectCode string
}
