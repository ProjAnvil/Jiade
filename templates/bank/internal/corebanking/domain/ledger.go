package domain

// DCFlag 借贷标志（复式记账核心）。
type DCFlag string

const (
	DCDebit  DCFlag = "借" // 资产、费用增加
	DCCredit DCFlag = "贷" // 负债、收入增加
)

// Subject 会计科目（对应 chart_of_acct 表）。
type Subject struct {
	Code       string
	Name       string
	DCAttr     DCFlag // 科目借贷属性
	Level      int
	ParentCode string
	Status     string
}

// Entry 一笔复式记账分录：哪个账户、借或贷、多少、入哪个科目。
type Entry struct {
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	SubjectCode string
}

// GLBalance 总账余额（对应 gl_balance 表）。
type GLBalance struct {
	SubjectCode string
	BizDate     string
	DCBalance   Money // 借方余额
	CCBalance   Money // 贷方余额
	Ccy         string
}

// BalanceDelta 某账户在某 biz_date 的余额增量（贷为正、借为负）。
// 过账时由 service 计算并交给 repo 累加。
type BalanceDelta struct {
	AccountNo   string
	Delta       Money
	SubjectCode string
}
