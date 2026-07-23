// Package domain is a pure domain model of the risk service, with zero DB/framework dependencies (innermost layer).
// risk has no amount field: risk_score/threshold is used as NUMERIC text direct deposit (without introducing Money).
package domain

// RiskRule corresponds to the risk_rule table.
type RiskRule struct {
	RuleID        string
	RuleName      string
	RuleType      string
	ConditionJSON string
	Threshold     string // NUMERIC(18,2) text, not amount (universal threshold)
	Action        string
	Status        string
}

// RiskEvent corresponds to the risk_event table.
type RiskEvent struct {
	EventID     string
	BizDate     string
	CustID      string
	AccountNo   string
	RuleID      string
	RiskScore   string // NUMERIC(6,2) text (0.30~0.95), not amount
	ActionTaken string
	TxnRef      string
	Summary     string
}

// Blacklist corresponds to the blacklist table.
type Blacklist struct {
	ListID           string
	CustID           string
	EntityType       string
	Reason           string
	EffectiveBizDate string
	ExpireDate       string
	Status           string
}

// RiskEventDetail is the event details aggregated by risk and customer services.
type RiskEventDetail struct {
	RiskEvent
	CustName string
	CustType string
}
