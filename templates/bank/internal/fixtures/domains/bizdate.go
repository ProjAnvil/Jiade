// Package domains 的 bizdate 子模块：多日切日引擎。
package domains

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
	"bank/internal/platform/pg"
)

// 节假日表（公历固定）。
var holidays = map[string]bool{
	"2025-10-01": true, "2025-10-02": true, "2025-10-03": true, // 国庆
	"2026-01-01": true, // 元旦
	"2026-02-16": true, "2026-02-17": true, "2026-02-18": true, // 春节
	"2025-10-10": true, "2025-11-11": true, // 双十一
}

func isHoliday(d time.Time) bool { return holidays[d.Format("2006-01-02")] }

// trendFactor 每月 +2%（base 2025-06-01）。
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

// cyclicalFactor 周末 ~60%。
func cyclicalFactor(d time.Time) float64 {
	if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
		return 0.60
	}
	return 1.0
}

// volumeForDay 当日交易笔数。每日独立 rng（seed+100+ordinal），factor=trend×seasonal×cyclical。
// 单日结果只依赖日期，子范围重跑可复现。
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

// parseDate2 解析 YYYY-MM-DD（返回 error，供 addDays 等需错误反馈处使用）。
func parseDate2(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
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

// addMonths 把 YYYY-MM-DD 加 n 月（n 可正可负；loan mature_date = start + term 用）。
func addMonths(dateStr string, n int) string {
	t, err := parseDate2(dateStr)
	if err != nil {
		return dateStr
	}
	return t.AddDate(0, n, 0).Format("2006-01-02")
}

// ---- 写入循环（批量 + 逐日幂等 + 切 sys_param）----

const bizDateBatchSize = 1000

// RunBizDate 从 StartBizDate 推进到 EndBizDate：每日 GenDay → 逐日 tx 内 DELETE 当日 + 批量 INSERT；
// 末尾 UPSERT sys_param.biz_date（=EndBizDate）/prev_biz_date（=次末日）。
func RunBizDate(ctx context.Context, db *sql.DB, cfg fixtures.Config, demandNos []string) error {
	if len(demandNos) == 0 {
		return fmt.Errorf("bizdate: 无账户")
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("bizdate: %w", err)
	}
	st := newDayState(cfg, demandNos)
	for _, d := range days {
		dayTxns, dayBalances := GenDay(cfg, d, demandNos, st)
		dateStr := d.Format("2006-01-02")
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM acct_txn WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日流水 %s: %w", dateStr, err)
			}
			if err := bulkInsertTxns(ctx, q, dayTxns); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM account_balance WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日余额 %s: %w", dateStr, err)
			}
			if err := bulkInsertBalances(ctx, q, dayBalances); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("bizdate: 写 %s 失败: %w", dateStr, err)
		}
	}
	end := days[len(days)-1].Format("2006-01-02")
	prev := days[len(days)-1].AddDate(0, 0, -1).Format("2006-01-02")
	if _, err := db.ExecContext(ctx, `INSERT INTO sys_param(param_key,param_value) VALUES ('biz_date',$1),('prev_biz_date',$2)
		ON CONFLICT (param_key) DO UPDATE SET param_value=EXCLUDED.param_value`, end, prev); err != nil {
		return fmt.Errorf("bizdate: 切 sys_param: %w", err)
	}
	return nil
}

// placeholders 生成 nRows×nCols 的 $N 占位符串：($1,$2),($3,$4),...
func placeholders(nRows, nCols int) string {
	var b strings.Builder
	idx := 1
	for r := 0; r < nRows; r++ {
		if r > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for c := 0; c < nCols; c++ {
			if c > 0 {
				b.WriteByte(',')
			}
			b.WriteString("$")
			b.WriteString(strconv.Itoa(idx))
			idx++
		}
		b.WriteByte(')')
	}
	return b.String()
}

// bulkInsertTxns 批量插 acct_txn（11 列；voucher_no/txn_status 走 DEFAULT ''/'normal'）。
func bulkInsertTxns(ctx context.Context, q pg.DBTX, rows []domain.Txn) error {
	const cols = 11
	const stmt = "INSERT INTO acct_txn(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, t := range chunk {
			args = append(args, t.TxnID, t.BizDate, t.AccountNo, string(t.DCFlag), t.Amount.String(),
				t.Ccy, t.SubjectCode, nullable(t.OppAccount), nullable(t.RefTxnID), nullable(t.Channel), nullable(t.Summary))
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("bizdate: 批量插流水: %w", err)
		}
	}
	return nil
}

// bulkInsertBalances 批量插 account_balance（6 列）。
func bulkInsertBalances(ctx context.Context, q pg.DBTX, rows []domain.Balance) error {
	const cols = 6
	const stmt = "INSERT INTO account_balance(account_no,biz_date,balance,available_balance,frozen_amount,subject_code) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, b := range chunk {
			args = append(args, b.AccountNo, b.BizDate, b.Balance.String(), b.AvailableBalance.String(),
				b.FrozenAmount.String(), b.SubjectCode)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("bizdate: 批量插余额: %w", err)
		}
	}
	return nil
}

// ---- 引擎内核（纯函数，无 DB）----

// DayState 账户余额的内存滚存态。
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
				b = domain.NewMoneyFromCents(0) // clamp 0（max(0,...) 口径）
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
