package fixtures

import (
	"math/rand/v2"
	"time"
)

// RNG Deterministic random source. Same as Seed → Same sequence (reproducible + single test hash comparison).
// PCG using math/rand/v2, fixed step size seed, zero-heavy dependencies.
type RNG struct {
	r *rand.Rand
}

// NewRNG uses seed to construct a deterministic RNG.
func NewRNG(seed int64) *RNG {
	return &RNG{r: rand.New(rand.NewPCG(uint64(seed), uint64(seed)))}
}

// IntRange Returns a random integer in [lo, hi] inclusive.
func (g *RNG) IntRange(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + g.r.IntN(hi-lo+1)
}

// Choice randomly selects one from the list.
func (g *RNG) Choice(list []string) string {
	return list[g.r.IntN(len(list))]
}

// Float64 Returns a deterministic random float in [0.0,1.0) (only used for non-amount generation like risk_score / factor scaling).
func (g *RNG) Float64() float64 { return g.r.Float64() }

// Handwritten small vocabulary (zh_CN semantics, zero external dependencies).
var (
	Surnames   = []string{"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "吴", "周"}
	GivenNames = []string{"伟", "芳", "娜", "秀英", "敏", "静", "磊", "强", "洋", "艳"}
	Branches   = []string{"HO", "SH", "BJ", "GZ", "CD"}
	Channels   = []string{"网银", "手机", "ATM", "柜面"}
	Summaries  = []string{"工资", "转账", "消费", "存款", "取款"}

	// B-1 New vocabulary library
	Genders           = []string{"M", "F"}
	RiskLevels        = []string{"low", "low", "low", "medium"} // 75% low
	CustRegions       = []string{"华东", "华北", "华南", "西南"}
	Industries        = []string{"A", "B", "C", "F", "G", "I", "K"}
	MCCs              = []string{"5411", "5912", "7011", "4111", "5310", "5732", "5812"}
	TransferSummaries = []string{"转账", "汇款", "还款"}
	CounterBanks      = []string{"本行", "他行"}
	Devices           = []string{"PC", "APP", "ATM", "柜台"}

	// B-4a New vocabulary library
	MemberLevelCodes = []string{"L1", "L2", "L3", "L4", "L5"}
	CampaignTypes    = []string{"满减", "返现", "积分翻倍", "新客"}
	PointSources     = []string{"消费", "活动", "签到", "赎回"}
	PointDirections  = []string{"earn", "earn", "earn", "redeem"} // 3/4 earn
	RiskActions      = []string{"拦截", "放行", "人工"}
	RiskReasons      = []string{"欺诈", "洗钱嫌疑", "投诉涉诉"}
	EntityTypes      = []string{"客户"}

	// B-4b New vocabulary library
	GuaranteeTypes = []string{"信用", "抵押", "保证"}
	OrderTypes     = []string{"申购", "申购", "赎回"} // 2/3 Subscription
	IncomeTypes    = []string{"利息"}
)

// LoanProduct Loan product tuple (CustType only tuple fidelity, loan_product table does not have this column).
type LoanProduct struct {
	Code, Name, LoanType, CustType string
	MinRate, MaxRate               float64 // Annualized rate (not amount)
	MaxTerm                        int     // moon
	MaxAmountYuan                  int     // Yuan (when writing the database ×100 rpm)
}

// LoanProducts 4 Loan Products.
var LoanProducts = []LoanProduct{
	{"LP-CONS", "个人消费贷", "消费", "个人", 0.0435, 0.0550, 36, 300000},
	{"LP-HOUS", "个人住房贷", "房贷", "个人", 0.0380, 0.0450, 360, 5000000},
	{"LP-OPER", "经营贷", "经营", "对公", 0.0450, 0.0600, 24, 2000000},
	{"LP-CRED", "信用贷", "消费", "个人", 0.0600, 0.0750, 12, 100000},
}

// OverdueClass Five overdue classification levels (based on the number of overdue days).
type OverdueClass struct {
	Days int
	Name string
}

// OverdueClasses 5-level threshold table (in ascending order of days).
var OverdueClasses = []OverdueClass{{0, "正常"}, {1, "关注"}, {30, "次级"}, {90, "可疑"}, {180, "损失"}}

// WealthProduct financial product tuple.
type WealthProduct struct {
	Code, Name, Type, Risk string
	ExpectedReturn         float64 // Annualized rate (not amount)
	MinAmountYuan          int     // Yuan
	TermDays               int
}

// WealthProducts 6 financial products.
var WealthProducts = []WealthProduct{
	{"WP-FIX1", "稳健固收1号", "固收", "低风险", 0.035, 1000, 365},
	{"WP-FIX3", "稳健固收3号", "固收", "中低", 0.040, 5000, 730},
	{"WP-MIX1", "平衡混合1号", "混合", "中", 0.065, 10000, 365},
	{"WP-EQT1", "成长股票1号", "权益", "中高", 0.085, 10000, 730},
	{"WP-MMO1", "现金管理1号", "货币", "低", 0.025, 100, 0},
	{"WP-FLX1", "灵活申赎1号", "货币", "低", 0.030, 1000, 0},
}

// RandomDate returns a deterministic random date in the interval [start,end] (YYYY-MM-DD).
func RandomDate(g *RNG, start, end string) string {
	t0, err := time.Parse("2006-01-02", start)
	if err != nil {
		return start
	}
	t1, err := time.Parse("2006-01-02", end)
	if err != nil {
		return end
	}
	days := int(t1.Sub(t0).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return t0.AddDate(0, 0, g.IntRange(0, days)).Format("2006-01-02")
}
