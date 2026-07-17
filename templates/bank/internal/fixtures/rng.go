package fixtures

import (
	"math/rand/v2"
	"time"
)

// RNG 确定性随机源。同 Seed → 同序列（可复现 + 单测哈希比对）。
// 用 math/rand/v2 的 PCG，定步长种子，零重依赖。
type RNG struct {
	r *rand.Rand
}

// NewRNG 用 seed 构造确定性 RNG。
func NewRNG(seed int64) *RNG {
	return &RNG{r: rand.New(rand.NewPCG(uint64(seed), uint64(seed)))}
}

// IntRange 返回 [lo, hi] 的随机整数（含两端）。
func (g *RNG) IntRange(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + g.r.IntN(hi-lo+1)
}

// Choice 从列表随机选一个。
func (g *RNG) Choice(list []string) string {
	return list[g.r.IntN(len(list))]
}

// Float64 返回 [0.0,1.0) 的确定性随机浮点（仅用于非金额生成，如 risk_score / factor 缩放）。
func (g *RNG) Float64() float64 { return g.r.Float64() }

// 手写小词库（对齐 bossy zh_CN 语义，零外部依赖）。
var (
	Surnames   = []string{"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "吴", "周"}
	GivenNames = []string{"伟", "芳", "娜", "秀英", "敏", "静", "磊", "强", "洋", "艳"}
	Branches   = []string{"HO", "SH", "BJ", "GZ", "CD"}
	Channels   = []string{"网银", "手机", "ATM", "柜面"}
	Summaries  = []string{"工资", "转账", "消费", "存款", "取款"}

	// B-1 新增词库
	Genders           = []string{"M", "F"}
	RiskLevels        = []string{"low", "low", "low", "medium"} // 75% low
	CustRegions       = []string{"华东", "华北", "华南", "西南"}
	Industries        = []string{"A", "B", "C", "F", "G", "I", "K"}
	MCCs              = []string{"5411", "5912", "7011", "4111", "5310", "5732", "5812"}
	TransferSummaries = []string{"转账", "汇款", "还款"}
	CounterBanks      = []string{"本行", "他行"}
	Devices           = []string{"PC", "APP", "ATM", "柜台"}

	// B-4a 新增词库
	MemberLevelCodes = []string{"L1", "L2", "L3", "L4", "L5"}
	CampaignTypes    = []string{"满减", "返现", "积分翻倍", "新客"}
	PointSources     = []string{"消费", "活动", "签到", "赎回"}
	PointDirections  = []string{"earn", "earn", "earn", "redeem"} // 3/4 earn
	RiskActions      = []string{"拦截", "放行", "人工"}
	RiskReasons      = []string{"欺诈", "洗钱嫌疑", "投诉涉诉"}
	EntityTypes      = []string{"客户"}

	// B-4b 新增词库
	GuaranteeTypes = []string{"信用", "抵押", "保证"}
	OrderTypes     = []string{"申购", "申购", "赎回"} // 2/3 申购
	IncomeTypes    = []string{"利息"}
)

// LoanProduct 贷款产品元组（移植 bossy loan.py PRODUCTS；CustType 仅元组保真，loan_product 表无此列）。
type LoanProduct struct {
	Code, Name, LoanType, CustType string
	MinRate, MaxRate               float64 // 年化比率（非金额）
	MaxTerm                        int     // 月
	MaxAmountYuan                  int     // 元（写库时 ×100 转分）
}

// LoanProducts bossy 4 贷款产品。
var LoanProducts = []LoanProduct{
	{"LP-CONS", "个人消费贷", "消费", "个人", 0.0435, 0.0550, 36, 300000},
	{"LP-HOUS", "个人住房贷", "房贷", "个人", 0.0380, 0.0450, 360, 5000000},
	{"LP-OPER", "经营贷", "经营", "对公", 0.0450, 0.0600, 24, 2000000},
	{"LP-CRED", "信用贷", "消费", "个人", 0.0600, 0.0750, 12, 100000},
}

// OverdueClass 逾期五级分类档位（按逾期天数，移植 bossy OVERDUE_CLASSES）。
type OverdueClass struct {
	Days int
	Name string
}

// OverdueClasses 5 档阈值表（天数升序）。
var OverdueClasses = []OverdueClass{{0, "正常"}, {1, "关注"}, {30, "次级"}, {90, "可疑"}, {180, "损失"}}

// WealthProduct 理财产品元组（移植 bossy wealth.py PRODUCTS）。
type WealthProduct struct {
	Code, Name, Type, Risk string
	ExpectedReturn         float64 // 年化比率（非金额）
	MinAmountYuan          int     // 元
	TermDays               int
}

// WealthProducts bossy 6 理财产品。
var WealthProducts = []WealthProduct{
	{"WP-FIX1", "稳健固收1号", "固收", "低风险", 0.035, 1000, 365},
	{"WP-FIX3", "稳健固收3号", "固收", "中低", 0.040, 5000, 730},
	{"WP-MIX1", "平衡混合1号", "混合", "中", 0.065, 10000, 365},
	{"WP-EQT1", "成长股票1号", "权益", "中高", 0.085, 10000, 730},
	{"WP-MMO1", "现金管理1号", "货币", "低", 0.025, 100, 0},
	{"WP-FLX1", "灵活申赎1号", "货币", "低", 0.030, 1000, 0},
}

// RandomDate 返回 [start,end]（YYYY-MM-DD）区间内的一个确定性随机日期。
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
