// Package contracts defines the protocol messages passed between RMB, the DCN
// application, and ADM over the message bus.
package contracts

// StepMessage is the sub-transaction message dispatched by the RMB coordinator to the DCN.
type StepMessage struct {
	TxID      string `json:"txId"`
	StepNo    int    `json:"stepNo"`
	Action    string `json:"action"` // DEBIT / CREDIT / COMPENSATE_DEBIT / COMPENSATE_CREDIT
	AccountID int    `json:"accountId"`
	Amount    string `json:"amount"`
}

// Receipt is the sub-transaction result reported by the DCN application back to the RMB coordinator.
type Receipt struct {
	TxID   string `json:"txId"`
	StepNo int    `json:"stepNo"`
	DCN    string `json:"dcn"`
	Status string `json:"status"` // DONE / FAILED
	Reason string `json:"reason,omitempty"`
}

// BalanceEvent is the balance-change event reported by the DCN application to ADM.
type BalanceEvent struct {
	TxID      string `json:"txId"`
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Direction string `json:"direction"` // DEBIT / CREDIT
	Amount    string `json:"amount"`
}

// StepDirection maps a sub-transaction action to (journal txId suffix, fund direction, is valid).
// Compensation actions derive their journal idempotency key with the ":comp" suffix, never colliding with the original sub-transaction.
func StepDirection(action string) (string, string, bool) {
	switch action {
	case "DEBIT":
		return "", "DEBIT", true
	case "CREDIT":
		return "", "CREDIT", true
	case "COMPENSATE_DEBIT":
		return ":comp", "CREDIT", true
	case "COMPENSATE_CREDIT":
		return ":comp", "DEBIT", true
	}
	return "", "", false
}

// ReverseAction returns the compensation action corresponding to an action.
func ReverseAction(action string) (string, bool) {
	switch action {
	case "DEBIT":
		return "COMPENSATE_DEBIT", true
	case "CREDIT":
		return "COMPENSATE_CREDIT", true
	}
	return "", false
}
