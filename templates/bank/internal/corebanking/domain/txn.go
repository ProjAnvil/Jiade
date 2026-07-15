package domain

// Txn 账务流水（对应 acct_txn 表）。
type Txn struct {
	TxnID       string
	BizDate     string
	TxnTs       string // timestamp 文本
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	Ccy         string
	SubjectCode string
	OppAccount  string
	RefTxnID    string
	Channel     string
	Summary     string
	VoucherNo   string    // 凭证号：一笔记账的所有分录共用
	TxnStatus   TxnStatus // normal / reversed
}

// Balance 分户账余额快照（对应 account_balance 表）。
type Balance struct {
	AccountNo        string
	BizDate          string
	Balance          Money
	AvailableBalance Money
	FrozenAmount     Money
	SubjectCode      string
}
