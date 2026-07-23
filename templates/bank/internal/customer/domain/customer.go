// Package domain is a pure domain model of customer service, with zero DB/framework dependencies (innermost layer).
package domain

// CustType Customer type.
type CustType string

const (
	CustTypePersonal CustType = "个人"
	CustTypeOrg      CustType = "对公"
)

// Customer corresponds to the cust_info table.
type Customer struct {
	CustID        string
	CustType      CustType
	Name          string
	CertType      string
	CertNo        string
	Gender        string // M/F; empty for public
	Birthday      string // YYYY-MM-DD; empty for public
	Nationality   string
	RiskLevel     string // low/medium
	KYCStatus     string // passed
	CreateBizDate string
}

// AccountRel corresponds to the cust_account_rel table (customer-account relationship).
type AccountRel struct {
	RelID     string
	CustID    string
	AccountNo string
	Role      string // Master/Co-owner
	RelType   string // Head of household
}

// CustAccount is the aggregation of the customer relationship and core-banking account information.
type CustAccount struct {
	AccountNo   string
	Ccy         string
	Status      string
	OpenBizDate string
	BranchCode  string
	Role        string
}
