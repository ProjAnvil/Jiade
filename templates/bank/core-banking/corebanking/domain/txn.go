package domain

// Txn accounting flow (corresponding to acct_txn table).
type Txn struct {
	TxnID       string
	BizDate     string
	TxnTs       string // timestamp text
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	Ccy         string
	SubjectCode string
	OppAccount  string
	RefTxnID    string
	Channel     string
	Summary     string
	VoucherNo   string    // Voucher number: shared by all entries in an accounting
	TxnStatus   TxnStatus // normal / reversed
}

// Balance Snapshot of the balance of the account (corresponding to the account_balance table).
type Balance struct {
	AccountNo        string
	BizDate          string
	Balance          Money
	AvailableBalance Money
	FrozenAmount     Money
	SubjectCode      string
}
