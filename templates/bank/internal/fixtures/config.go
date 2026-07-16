// Package fixtures 是 bank 工程的确定性 fixture 生成器。
package fixtures

// Scale 规模。
type Scale string

const (
	ScaleDev  Scale = "dev"
	ScaleFull Scale = "full"
)

// Counts 各实体的目标量级。
type Counts struct {
	Customers      int
	DemandAccounts int
	FixedAccounts  int
	DailyTxnLo     int
	DailyTxnHi     int
}

// DEV 约为 FULL 的 1/4（对齐 bossy TARGET_COUNTS）。
var targetCounts = map[Scale]Counts{
	ScaleDev:  {Customers: 1250, DemandAccounts: 2000, FixedAccounts: 500, DailyTxnLo: 500, DailyTxnHi: 1250},
	ScaleFull: {Customers: 5000, DemandAccounts: 8000, FixedAccounts: 2000, DailyTxnLo: 2000, DailyTxnHi: 5000},
}

// Config fixture 配置。
type Config struct {
	StartBizDate string // YYYY-MM-DD
	EndBizDate   string
	Scale        Scale
	Seed         int64
}

// DefaultConfig 按规模给默认（对齐 bossy：start 2025-06-01, end 2026-07-13, seed 42）。
func DefaultConfig(scale Scale) Config {
	return Config{StartBizDate: "2025-06-01", EndBizDate: "2026-07-13", Scale: scale, Seed: 42}
}

// TargetCounts 返回当前规模的目标量级。
func (c Config) TargetCounts() Counts {
	if tc, ok := targetCounts[c.Scale]; ok {
		return tc
	}
	return targetCounts[ScaleDev]
}

// ScaleFactor 返回规模缩放（full=1.0, dev=0.25），移植 bossy scale_factor。
// reward/risk/loan/wealth 的每日量 = base × ScaleFactor × factor。
func ScaleFactor(s Scale) float64 {
	if s == ScaleFull {
		return 1.0
	}
	return 0.25
}
