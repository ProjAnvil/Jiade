// Package domains 的 bizdate 子模块：多日切日引擎（移植 bossy bizdate+distribution）。
package domains

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
)

// bossy 节假日（公历固定，移植自 distribution.py HOLIDAYS）。
var holidays = map[string]bool{
	"2025-10-01": true, "2025-10-02": true, "2025-10-03": true, // 国庆
	"2026-01-01": true, // 元旦
	"2026-02-16": true, "2026-02-17": true, "2026-02-18": true, // 春节
	"2025-10-10": true, "2025-11-11": true, // 双十一
}

func isHoliday(d time.Time) bool { return holidays[d.Format("2006-01-02")] }

// trendFactor 每月 +2%（base 2025-06-01，移植 bossy trend_factor）。
func trendFactor(d time.Time) float64 {
	const baseYear, baseMonth = 2025, 6
	months := (d.Year()-baseYear)*12 + int(d.Month()) - baseMonth
	return 1.0 + 0.02*float64(months)
}

// seasonalFactor 季末×1.35 / 年末×1.50 / 发薪日×1.40 / 节假日×1.30（乘性叠加）。
func seasonalFactor(d time.Time) float64 {
	f := 1.0
	m := int(d.Month())
	if (m == 3 || m == 6 || m == 9 || m == 12) && d.Day() >= 28 { // 季末考核
		f *= 1.35
	}
	if m == 12 && d.Day() >= 25 { // 年末 surge
		f *= 1.50
	}
	if d.Day() == 10 || d.Day() == 15 { // 发薪日
		f *= 1.40
	}
	if isHoliday(d) { // 节假日消费高峰
		f *= 1.30
	}
	return f
}

// cyclicalFactor 周末 ~60%（移植 bossy cyclical_factor）。
func cyclicalFactor(d time.Time) float64 {
	if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		return 0.60
	}
	return 1.0
}

// volumeForDay 当日交易笔数。每日独立 rng（seed+100+ordinal），factor=trend×seasonal×cyclical。
// 单日结果只依赖日期，子范围重跑可复现（对齐 bossy volume_fn）。
func volumeForDay(cfg fixtures.Config, d time.Time) int {
	base := parseDate(cfg.StartBizDate)
	rng := fixtures.NewRNG(cfg.Seed + 100 + dayOrdinal(d, base))
	factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
	tc := cfg.TargetCounts()
	lo := int(float64(tc.DailyTxnLo) * factor)
	if lo < 1 {
		lo = 1
	}
	hi := int(float64(tc.DailyTxnHi) * factor)
	if hi < lo+1 {
		hi = lo + 1
	}
	return rng.IntRange(lo, hi)
}

// ---- 日期 helper ----

func parseDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// dayOrdinal d 自 base 起的天数（含 0）。
func dayOrdinal(d, base time.Time) int64 {
	return int64(d.Sub(base).Hours() / 24)
}

// dateCompact "2026-07-16" → "20260716"（txn_id 用）。
func dateCompact(d time.Time) string {
	var b strings.Builder
	for _, c := range d.Format("2006-01-02") {
		if c != '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// bizDateRange [start,end]（YYYY-MM-DD，含两端）的日历日列表。
func bizDateRange(start, end string) ([]time.Time, error) {
	t0, t1 := parseDate(start), parseDate(end)
	if t0.IsZero() || t1.IsZero() {
		return nil, fmt.Errorf("bizdate: 非法日期 %s~%s", start, end)
	}
	if t1.Before(t0) {
		return nil, fmt.Errorf("bizdate: end<start %s~%s", start, end)
	}
	var out []time.Time
	for d := t0; !d.After(t1); d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out, nil
}

// 占位：避免 strconv 未用 import 报错（Task 3 的 placeholders 会用到 strconv）。
var _ = strconv.Itoa

// ---- 引擎内核（纯函数，无 DB）----

// DayState 账户余额的内存滚存态（对齐 bossy DailyState）。
type DayState struct{ Bal map[string]domain.Money }

// newDayState 初始化每账户余额（rng seed+2，回收 Spec A 已删 GenBalanceRows 的偏移）。
func newDayState(cfg fixtures.Config, demandNos []string) *DayState {
	rng := fixtures.NewRNG(cfg.Seed + 2)
	st := &DayState{Bal: make(map[string]domain.Money, len(demandNos))}
	for _, no := range demandNos {
		st.Bal[no] = domain.NewMoneyFromCents(int64(rng.IntRange(1, 99999)) * 10000)
	}
	return st
}

// GenDay 生成当日流水 + 当日全账户余额快照，推进 st。纯函数。
// 每日独立内容 rng（seed+200+ordinal）；流水贷多借少→余额温和增长；txn_id 确定。
func GenDay(cfg fixtures.Config, date time.Time, demandNos []string, st *DayState) (dayTxns []domain.Txn, dayBalances []domain.Balance) {
	if len(demandNos) == 0 {
		return nil, nil
	}
	n := volumeForDay(cfg, date)
	rng := fixtures.NewRNG(cfg.Seed + 200 + dayOrdinal(date, parseDate(cfg.StartBizDate)))
	dateStr := date.Format("2006-01-02")
	compact := dateCompact(date)

	dayTxns = make([]domain.Txn, 0, n)
	for i := 0; i < n; i++ {
		acct := demandNos[rng.IntRange(0, len(demandNos)-1)]
		amt := domain.NewMoneyFromCents(int64(rng.IntRange(1, 9999)) * 1000)
		credit := rng.IntRange(0, 2) < 2 // 0/1=贷(2/3)，2=借(1/3)
		var dc domain.DCFlag
		if credit {
			dc = domain.DCCredit
			st.Bal[acct] = st.Bal[acct].Add(amt)
		} else {
			dc = domain.DCDebit
			b := st.Bal[acct].Sub(amt)
			if b < 0 {
				b = domain.NewMoneyFromCents(0) // clamp 0（对齐 bossy max(0,...)）
			}
			st.Bal[acct] = b
		}
		dayTxns = append(dayTxns, domain.Txn{
			TxnID:       fmt.Sprintf("T%s-%05d", compact, i),
			BizDate:     dateStr,
			AccountNo:   acct,
			DCFlag:      dc,
			Amount:      amt,
			Ccy:         "CNY",
			SubjectCode: "2011",
			OppAccount:  demandNos[rng.IntRange(0, len(demandNos)-1)],
			Channel:     rng.Choice(fixtures.Channels),
			Summary:     rng.Choice(fixtures.Summaries),
			VoucherNo:   "",
			TxnStatus:   domain.TxnStatusNormal,
		})
	}

	dayBalances = make([]domain.Balance, 0, len(demandNos))
	for _, no := range demandNos { // 按 demandNos 顺序快照（确定性，不 range map）
		bal := st.Bal[no]
		dayBalances = append(dayBalances, domain.Balance{
			AccountNo: no, BizDate: dateStr,
			Balance: bal, AvailableBalance: bal,
			FrozenAmount: domain.NewMoneyFromCents(0), SubjectCode: "2011",
		})
	}
	return dayTxns, dayBalances
}
