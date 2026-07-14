package fixtures

import "math/rand/v2"

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

// 手写小词库（对齐 bossy zh_CN 语义，零外部依赖）。
var (
	Surnames   = []string{"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "吴", "周"}
	GivenNames = []string{"伟", "芳", "娜", "秀英", "敏", "静", "磊", "强", "洋", "艳"}
	Branches   = []string{"HO", "SH", "BJ", "GZ", "CD"}
	Channels   = []string{"网银", "手机", "ATM", "柜面"}
	Summaries  = []string{"工资", "转账", "消费", "存款", "取款"}
)
