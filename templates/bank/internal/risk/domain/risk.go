// Package domain 是 risk 服务的纯领域模型，零 DB/框架依赖（最内层）。
// risk 无金额字段：risk_score/threshold 作 NUMERIC 文本直存（不引入 Money）。
package domain

// RiskRule 对应 risk_rule 表。
type RiskRule struct {
	RuleID        string
	RuleName      string
	RuleType      string
	ConditionJSON string
	Threshold     string // NUMERIC(18,2) 文本，非金额（通用阈值）
	Action        string
	Status        string
}

// RiskEvent 对应 risk_event 表。
type RiskEvent struct {
	EventID     string
	BizDate     string
	CustID      string
	AccountNo   string
	RuleID      string
	RiskScore   string // NUMERIC(6,2) 文本（0.30~0.95），非金额
	ActionTaken string
	TxnRef      string
	Summary     string
}

// Blacklist 对应 blacklist 表。
type Blacklist struct {
	ListID           string
	CustID           string
	EntityType       string
	Reason           string
	EffectiveBizDate string
	ExpireDate       string
	Status           string
}

// RiskEventDetail 是联邦查询结果（risk_event JOIN ext_cust_db_cust_info）。
type RiskEventDetail struct {
	RiskEvent
	CustName string
	CustType string
}
