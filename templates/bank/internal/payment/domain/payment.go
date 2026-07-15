package domain

// Transfer corresponds to the transfer_txn table.
type Transfer struct {
	TxnID       string
	BizDate     string
	OutAccount  string
	InAccount   string
	Amount      Money
	Ccy         string
	Fee         Money
	Channel     string
	CounterBank string
	Status      string
	Summary     string
}

// Consumption corresponds to the consumption_txn table.
type Consumption struct {
	TxnID      string
	BizDate    string
	AccountNo  string
	MerchantID string
	MCC        string
	Amount     Money
	Ccy        string
	Status     string
	Summary    string
}

// Merchant corresponds to the merchant table.
type Merchant struct {
	MerchantID    string
	MerchantName  string
	MCC           string
	Region        string
	Status        string
	CreateBizDate string
}

// ChannelTxn corresponds to the channel_txn table.
type ChannelTxn struct {
	TxnID     string
	BizDate   string
	Channel   string
	Device    string
	CustID    string
	Status    string
	LatencyMs int
}

// FeeRecord corresponds to the fee_record table.
type FeeRecord struct {
	FeeID        string
	BizDate      string
	TxnID        string
	FeeType      string
	Amount       Money
	Ccy          string
	PayOrReceive string
}

// Settlement corresponds to the settlement_record table.
type Settlement struct {
	SettleID  string
	BizDate   string
	Channel   string
	NetAmount Money
	TxnCount  int
	Status    string
}

// TransferParty is a federated JOIN result from transfer_txn (account + customer name).
type TransferParty struct {
	TxnID       string
	Amount      Money
	Ccy         string
	OutAccount  string
	OutCustName string
	InAccount   string
	InCustName  string
	BizDate     string
}
