// Package domain 是 reward 服务的纯领域模型，零 DB/框架依赖（最内层）。
package domain

// PointsAcct 对应 points_acct 表。
type PointsAcct struct {
	CustID         string
	PointsBalance  int
	FrozenPoints   int
	MemberLevel    string
	UpdateBizDate  string
}

// PointsTxn 对应 points_txn 表。
type PointsTxn struct {
	TxnID      string
	CustID     string
	BizDate    string
	Points     int
	Direction  string // earn/redeem
	SourceType string
	RefTxnID   string
	Summary    string
}

// Coupon 对应 coupon 表（face_value/min_spend 为金额 int64 分）。
type Coupon struct {
	CouponID     string
	CustID       string
	CampaignID   string
	FaceValue    Money
	MinSpend     Money
	Status       string
	IssueBizDate string
	ExpireDate   string
}

// Campaign 对应 campaign 表（budget/used_budget 为金额）。
type Campaign struct {
	CampaignID    string
	Name          string
	Type          string
	StartBizDate  string
	EndBizDate    string
	Budget        Money
	UsedBudget    Money
	Status        string
}

// MemberLevel 对应 member_level 表。
type MemberLevel struct {
	LevelCode       string
	LevelName       string
	PointsThreshold int
	BenefitsJSON    string
}

// RewardProfile 是联邦查询结果（points_acct JOIN ext_cust_db_cust_info）。
type RewardProfile struct {
	CustID        string
	PointsBalance int
	MemberLevel   string
	CustName      string
	CustType      string
}
