// Package fixtures are deterministic fixture generators for bank projects.
package fixtures

// Scale scale.
type Scale string

const (
	ScaleDev  Scale = "dev"
	ScaleFull Scale = "full"
)

// Counts Target magnitude for each entity.
type Counts struct {
	Customers      int
	DemandAccounts int
	FixedAccounts  int
	DailyTxnLo     int
	DailyTxnHi     int
}

// DEV is about 1/4 of FULL.
var targetCounts = map[Scale]Counts{
	ScaleDev:  {Customers: 1250, DemandAccounts: 2000, FixedAccounts: 500, DailyTxnLo: 500, DailyTxnHi: 1250},
	ScaleFull: {Customers: 5000, DemandAccounts: 8000, FixedAccounts: 2000, DailyTxnLo: 2000, DailyTxnHi: 5000},
}

// Config fixture configuration.
type Config struct {
	StartBizDate string // YYYY-MM-DD
	EndBizDate   string
	Scale        Scale
	Seed         int64
}

// DefaultConfig gives the default value based on scale (start 2025-06-01, end 2026-07-13, seed 42).
func DefaultConfig(scale Scale) Config {
	return Config{StartBizDate: "2025-06-01", EndBizDate: "2026-07-13", Scale: scale, Seed: 42}
}

// TargetCounts Returns the target magnitudes for the current scale.
func (c Config) TargetCounts() Counts {
	if tc, ok := targetCounts[c.Scale]; ok {
		return tc
	}
	return targetCounts[ScaleDev]
}

// ScaleFactor Returns the scale scale (full=1.0, dev=0.25).
// Daily amount of reward/risk/loan/wealth = base × ScaleFactor × factor.
func ScaleFactor(s Scale) float64 {
	if s == ScaleFull {
		return 1.0
	}
	return 0.25
}
