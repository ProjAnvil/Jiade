// Package domain is a pure domain model of wealth service, with zero DB/framework dependencies (innermost layer).
// The amount field (cost/current_value/amount/min_amount) uses Money (int64 points);
// nav/accum_nav/share/expected_return is a non-monetary decimal, NUMERIC text stored directly (aligned to the risk_score boundary of risk).
package domain

// WealthProduct corresponds to the wealth_product table.
type WealthProduct struct {
	ProductCode    string
	ProductName    string
	ProductType    string
	RiskLevel      string
	ExpectedReturn string // NUMERIC(10,6) text (ratio, not amount)
	MinAmount      Money
	TermDays       int
	StartBizDate   string
	EndBizDate     string
	Status         string
}

// WealthNav corresponds to the wealth_nav table (daily full net worth snapshot).
type WealthNav struct {
	ProductCode string
	BizDate     string
	Nav         string // NUMERIC(12,6) text (not amount)
	AccumNav    string // NUMERIC(12,6) text (not amount)
}

// WealthHolding corresponds to the wealth_holding table.
type WealthHolding struct {
	HoldingID    string
	CustID       string
	AccountNo    string
	ProductCode  string
	Ccy          string
	Share        string // NUMERIC(18,4) text (not amount)
	Cost         Money
	CurrentValue Money
	BizDate      string
}

// WealthOrder corresponds to the wealth_order table.
type WealthOrder struct {
	OrderID     string
	BizDate     string
	CustID      string
	ProductCode string
	AccountNo   string
	OrderType   string
	Amount      Money
	Share       string // NUMERIC(18,4) text (not amount)
	Nav         string // NUMERIC(12,6) text (not amount)
	Status      string
}

// WealthIncome corresponds to the wealth_income table (B-4b Q1-B daily interest).
type WealthIncome struct {
	IncomeID   string
	BizDate    string
	HoldingID  string
	IncomeType string
	Amount     Money
}

// WealthProfile is a position profile aggregated by wealth and customer services.
type WealthProfile struct {
	HoldingID    string
	CustID       string
	ProductCode  string
	Share        string
	CurrentValue Money
	CustName     string
	CustType     string
}
