package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// Action Accounting business action.
type Action string

const (
	ActionDeposit  Action = "deposit"  // Deposit
	ActionWithdraw Action = "withdraw" // Withdraw
	ActionTransfer Action = "transfer" // transfer
)

// ReverseMode Reverse mode.
type ReverseMode string

const (
	ReverseBlue ReverseMode = "blue" // Blue rush: change status + reverse delta rollback, no new turnover
	ReverseRed  ReverseMode = "red"  // Red flush: Reverse entry goes to Post, new reverse flow is added
)

// TxnStatus Pipeline status.
type TxnStatus string

const (
	TxnStatusNormal   TxnStatus = "normal"
	TxnStatusReversed TxnStatus = "reversed"
)

// CashSubject Inventory cash account (counterparty account for deposits/withdrawals).
const CashSubject = "1001"

// The result of Booking's accounting (one voucher): voucher number + all double-entry statements under it.
type Booking struct {
	VoucherNo string
	BizDate   string
	Txns      []Txn
}

// NewVoucherNo generates a voucher number: V + bizDate (remove horizontal lines) + 16-digit random hex. bizDate is in the form of "2026-07-16".
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
