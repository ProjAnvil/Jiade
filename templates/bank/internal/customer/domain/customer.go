// Package domain 是 customer 服务的纯领域模型，零 DB/框架依赖（最内层）。
package domain

// CustType 客户类型。
type CustType string

const (
	CustTypePersonal CustType = "个人"
	CustTypeOrg      CustType = "对公"
)

// Customer 对应 cust_info 表。
type Customer struct {
	CustID        string
	CustType      CustType
	Name          string
	CertType      string
	CertNo        string
	Gender        string // M/F；对公为空
	Birthday      string // YYYY-MM-DD；对公为空
	Nationality   string
	RiskLevel     string // low/medium
	KYCStatus     string // passed
	CreateBizDate string
}

// AccountRel 对应 cust_account_rel 表（客户-账户关系）。
type AccountRel struct {
	RelID     string
	CustID    string
	AccountNo string
	Role      string // 主/共
	RelType   string // 户主
}

// CustAccount 是跨库联邦查询结果（cust_account_rel JOIN ext_core_db_demand_account）。
type CustAccount struct {
	AccountNo   string
	Ccy         string
	Status      string
	OpenBizDate string
	BranchCode  string
	Role        string
}
