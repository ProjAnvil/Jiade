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
)

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
