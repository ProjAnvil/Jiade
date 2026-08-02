// Package contracts 定义经消息总线在 RMB / DCN 应用 / ADM 之间传递的协议消息。
package contracts

// StepMessage 是 RMB 协调服务下发给 DCN 的子事务消息。
type StepMessage struct {
	TxID      string `json:"txId"`
	StepNo    int    `json:"stepNo"`
	Action    string `json:"action"` // DEBIT / CREDIT / COMPENSATE_DEBIT / COMPENSATE_CREDIT
	AccountID int    `json:"accountId"`
	Amount    string `json:"amount"`
}

// Receipt 是 DCN 应用回执给 RMB 协调服务的子事务结果。
type Receipt struct {
	TxID   string `json:"txId"`
	StepNo int    `json:"stepNo"`
	DCN    string `json:"dcn"`
	Status string `json:"status"` // DONE / FAILED
	Reason string `json:"reason,omitempty"`
}

// BalanceEvent 是 DCN 应用上报给 ADM 的余额变更事件。
type BalanceEvent struct {
	TxID      string `json:"txId"`
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Direction string `json:"direction"` // DEBIT / CREDIT
	Amount    string `json:"amount"`
}

// StepDirection 把子事务动作映射为 (journal txId 后缀, 资金方向, 是否合法)。
// 补偿动作用 ":comp" 后缀派生 journal 幂等键，与原始子事务互不冲突。
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

// ReverseAction 返回动作对应的补偿动作。
func ReverseAction(action string) (string, bool) {
	switch action {
	case "DEBIT":
		return "COMPENSATE_DEBIT", true
	case "CREDIT":
		return "COMPENSATE_CREDIT", true
	}
	return "", false
}
