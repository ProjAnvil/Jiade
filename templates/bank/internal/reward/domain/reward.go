// Package domain is a pure domain model of the reward service, with zero DB/framework dependencies (innermost layer).
package domain

// PointsAcct corresponds to the points_acct table.
type PointsAcct struct {
	CustID        string
	PointsBalance int
	FrozenPoints  int
	MemberLevel   string
	UpdateBizDate string
}

// PointsTxn corresponds to the points_txn table.
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

// Coupon corresponds to the coupon table (face_value/min_spend is the amount int64 points).
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

// Campaign corresponds to the campaign table (budget/used_budget is the amount).
type Campaign struct {
	CampaignID   string
	Name         string
	Type         string
	StartBizDate string
	EndBizDate   string
	Budget       Money
	UsedBudget   Money
	Status       string
}

// MemberLevel corresponds to the member_level table.
type MemberLevel struct {
	LevelCode       string
	LevelName       string
	PointsThreshold int
	BenefitsJSON    string
}

// RewardProfile is a points profile aggregated by reward and customer services.
type RewardProfile struct {
	CustID        string
	PointsBalance int
	MemberLevel   string
	CustName      string
	CustType      string
}
