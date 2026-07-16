package domains

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
	"bank/internal/risk/domain"
)

// risk 规则（移植 bossy risk.py RULES）。field/op/threshold 编进 condition_json。
var riskRules = []struct {
	ID, Name, Field string
	Threshold       int
	Action          string
}{
	{"R001", "单笔大额转账", "amount", 100000, "拦截"},
	{"R002", "频繁交易", "count_5min", 10, "人工"},
	{"R003", "异地登录", "region_mismatch", 1, "放行"},
	{"R004", "非工作时间大额", "amount+hour", 50000, "人工"},
	{"R005", "黑名单命中", "blacklist", 1, "拦截"},
}

// RiskStatic 静态表行集合。
type RiskStatic struct {
	Rules      []domain.RiskRule
	Blacklists []domain.Blacklist
}

// GenRiskStatic 生成 risk_rule + blacklist。rng 偏移 +32。
func GenRiskStatic(cfg fixtures.Config, custIDs []string) RiskStatic {
	rng := fixtures.NewRNG(cfg.Seed + 32)
	sf := fixtures.ScaleFactor(cfg.Scale)

	rules := make([]domain.RiskRule, len(riskRules))
	for i, rr := range riskRules {
		cond, _ := json.Marshal(map[string]any{"field": rr.Field, "op": ">=", "threshold": rr.Threshold})
		rules[i] = domain.RiskRule{
			RuleID: rr.ID, RuleName: rr.Name, RuleType: "transaction",
			ConditionJSON: string(cond), Threshold: fmt.Sprintf("%d.00", rr.Threshold),
			Action: rr.Action, Status: "active",
		}
	}

	blCount := maxInt(2, int(20*sf))
	blacklists := make([]domain.Blacklist, blCount)
	for i := 0; i < blCount; i++ {
		blacklists[i] = domain.Blacklist{
			ListID: fmt.Sprintf("RS-BL-%04d", i), CustID: pickStr(rng, custIDs),
			EntityType: rng.Choice(fixtures.EntityTypes), Reason: rng.Choice(fixtures.RiskReasons),
			EffectiveBizDate: cfg.StartBizDate, ExpireDate: cfg.EndBizDate, Status: "active",
		}
	}
	return RiskStatic{Rules: rules, Blacklists: blacklists}
}

// WriteRiskStatic 幂等写 risk_rule + blacklist（先 DELETE 后 INSERT）。
func WriteRiskStatic(ctx context.Context, db *sql.DB, s RiskStatic) error {
	for _, t := range []string{"blacklist", "risk_rule"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, r := range s.Rules {
		if _, err := db.ExecContext(ctx, `INSERT INTO risk_rule(rule_id,rule_name,rule_type,condition_json,threshold,action,status)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			r.RuleID, r.RuleName, r.RuleType, r.ConditionJSON, nullable(r.Threshold), r.Action, r.Status); err != nil {
			return err
		}
	}
	for _, b := range s.Blacklists {
		if _, err := db.ExecContext(ctx, `INSERT INTO blacklist(list_id,cust_id,entity_type,reason,effective_biz_date,expire_date,status)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			b.ListID, nullable(b.CustID), b.EntityType, b.Reason, b.EffectiveBizDate, b.ExpireDate, b.Status); err != nil {
			return err
		}
	}
	return nil
}

// RunRisk 按 bizDate 推进生成 risk_event（逐日三因子 + 每日独立 rng seed+33+ordinal）。
func RunRisk(ctx context.Context, db *sql.DB, cfg fixtures.Config, custIDs, accountNos []string) error {
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("risk: %w", err)
	}
	sf := fixtures.ScaleFactor(cfg.Scale)
	ruleIDs := make([]string, len(riskRules))
	for i, r := range riskRules {
		ruleIDs[i] = r.ID
	}
	if len(custIDs) == 0 {
		custIDs = []string{""}
	}
	if len(accountNos) == 0 {
		accountNos = []string{""}
	}
	base := parseDate(cfg.StartBizDate)
	for _, d := range days {
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		n := maxInt(0, int(5*sf*factor))
		rng := fixtures.NewRNG(cfg.Seed + 33 + dayOrdinal(d, base))
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		events := make([]domain.RiskEvent, 0, n)
		for i := 0; i < n; i++ {
			ruleID := pickStr(rng, ruleIDs) // 同一 rule id 复用于 RuleID 与 Summary，避免文案/规则错配
			events = append(events, domain.RiskEvent{
				EventID: fmt.Sprintf("RS-EV-%s-%05d", compact, i), BizDate: dateStr,
				CustID: pickStr(rng, custIDs), AccountNo: pickStr(rng, accountNos),
				RuleID: ruleID,
				RiskScore: fmt.Sprintf("%.2f", 0.3+rng.Float64()*0.65),
				ActionTaken: rng.Choice(fixtures.RiskActions),
				TxnRef:  fmt.Sprintf("RS-TX-%s-%05d", compact, i),
				Summary: "触发规则 " + ruleID,
			})
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM risk_event WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 risk_event %s: %w", dateStr, err)
			}
			return bulkInsertRiskEvents(ctx, q, events)
		}); err != nil {
			return fmt.Errorf("risk: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// bulkInsertRiskEvents 批量插 risk_event（9 列；cust/account/rule/txn_ref/summary nullable）。
func bulkInsertRiskEvents(ctx context.Context, q pg.DBTX, rows []domain.RiskEvent) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 9
	const stmt = "INSERT INTO risk_event(event_id,biz_date,cust_id,account_no,rule_id,risk_score,action_taken,txn_ref,summary) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, e := range chunk {
			args = append(args, e.EventID, e.BizDate, nullable(e.CustID), nullable(e.AccountNo),
				nullable(e.RuleID), nullable(e.RiskScore), nullable(e.ActionTaken),
				nullable(e.TxnRef), nullable(e.Summary))
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("risk: 批量插 risk_event: %w", err)
		}
	}
	return nil
}

// pickStr 从 list 随机选一个（空 list 返回 ""）。
func pickStr(rng *fixtures.RNG, list []string) string {
	if len(list) == 0 {
		return ""
	}
	return list[rng.IntRange(0, len(list)-1)]
}
