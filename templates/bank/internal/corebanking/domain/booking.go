package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// Action 记账业务动作。
type Action string

const (
	ActionDeposit  Action = "deposit"  // 存入
	ActionWithdraw Action = "withdraw" // 支取
	ActionTransfer Action = "transfer" // 转账
)

// ReverseMode 冲正模式。
type ReverseMode string

const (
	ReverseBlue ReverseMode = "blue" // 蓝冲：改状态 + 逆向 delta 回滚，不新增流水
	ReverseRed  ReverseMode = "red"  // 红冲：反向分录走 Post，新增反向流水
)

// TxnStatus 流水状态。
type TxnStatus string

const (
	TxnStatusNormal   TxnStatus = "normal"
	TxnStatusReversed TxnStatus = "reversed"
)

// CashSubject 库存现金科目（存款/取款的对方科目）。
const CashSubject = "1001"

// Booking 一笔记账的结果（一张凭证）：凭证号 + 其下所有复式流水。
type Booking struct {
	VoucherNo string
	BizDate   string
	Txns      []Txn
}

// NewVoucherNo 生成凭证号：V + bizDate(去横线) + 16位随机 hex。bizDate 形如 "2026-07-16"。
func NewVoucherNo(bizDate string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	compact := ""
	for _, c := range bizDate {
		if c != '-' {
			compact += string(c)
		}
	}
	return "V" + compact + hex.EncodeToString(b)
}
