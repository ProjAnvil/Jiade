// Package domain 是 wealth 服务的纯领域模型，零 DB/框架依赖（最内层）。
// 金额字段（cost/current_value/amount/min_amount）用 Money（int64 分）；
// nav/accum_nav/share/expected_return 是非货币小数，NUMERIC 文本直存（对齐 risk 的 risk_score 边界）。
package domain

// WealthProduct 对应 wealth_product 表。
type WealthProduct struct {
	ProductCode    string
	ProductName    string
	ProductType    string
	RiskLevel      string
	ExpectedReturn string // NUMERIC(10,6) 文本（比率，非金额）
	MinAmount      Money
	TermDays       int
	StartBizDate   string
	EndBizDate     string
	Status         string
}

// WealthNav 对应 wealth_nav 表（逐日全量净值快照）。
type WealthNav struct {
	ProductCode string
	BizDate     string
	Nav         string // NUMERIC(12,6) 文本（非金额）
	AccumNav    string // NUMERIC(12,6) 文本（非金额）
}

// WealthHolding 对应 wealth_holding 表。
type WealthHolding struct {
	HoldingID    string
	CustID       string
	AccountNo    string
	ProductCode  string
	Ccy          string
	Share        string // NUMERIC(18,4) 文本（非金额）
	Cost         Money
	CurrentValue Money
	BizDate      string
}

// WealthOrder 对应 wealth_order 表。
type WealthOrder struct {
	OrderID     string
	BizDate     string
	CustID      string
	ProductCode string
	AccountNo   string
	OrderType   string
	Amount      Money
	Share       string // NUMERIC(18,4) 文本（非金额）
	Nav         string // NUMERIC(12,6) 文本（非金额）
	Status      string
}

// WealthIncome 对应 wealth_income 表（B-4b Q1-B 每日利息）。
type WealthIncome struct {
	IncomeID   string
	BizDate    string
	HoldingID  string
	IncomeType string
	Amount     Money
}

// WealthProfile 是联邦查询结果（wealth_holding JOIN ext_cust_db_cust_info）。
type WealthProfile struct {
	HoldingID    string
	CustID       string
	ProductCode  string
	Share        string
	CurrentValue Money
	CustName     string
	CustType     string
}
