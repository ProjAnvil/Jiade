// Package repo 是 risk 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/risk/domain"
)

// RiskRepo risk 仓储。本库 risk_event/risk_rule/blacklist 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type RiskRepo struct{ db *sql.DB }

// NewRiskRepo 构造 RiskRepo。
func NewRiskRepo(db *sql.DB) *RiskRepo { return &RiskRepo{db: db} }

// ListEvents 按日期/规则/action 筛选（空则不限），分页。
func (r *RiskRepo) ListEvents(ctx context.Context, from, to, ruleID, action string, offset, limit int) ([]domain.RiskEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT event_id,biz_date,cust_id,account_no,rule_id,risk_score,action_taken,txn_ref,summary
		FROM risk_event WHERE (NULLIF($1,'') IS NULL OR biz_date >= NULLIF($1,'')::date)
		AND (NULLIF($2,'') IS NULL OR biz_date <= NULLIF($2,'')::date)
		AND ($3='' OR rule_id=$3) AND ($4='' OR action_taken=$4)
		ORDER BY biz_date DESC, event_id LIMIT $5 OFFSET $6`, from, to, ruleID, action, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列风控事件: %w", err)
	}
	defer rows.Close()
	var out []domain.RiskEvent
	for rows.Next() {
		e, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo: 列风控事件 scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列风控事件: %w", err)
	}
	return out, nil
}

// GetEvent 跨库联邦：risk_event JOIN ext_cust_db_cust_info → 事件详情 + 客户姓名/类型。
func (r *RiskRepo) GetEvent(ctx context.Context, eventID string) (domain.RiskEventDetail, error) {
	q := `SELECT e.event_id,e.biz_date,e.cust_id,e.account_no,e.rule_id,e.risk_score,e.action_taken,e.txn_ref,e.summary,ci.name,ci.cust_type
		FROM risk_event e
		LEFT JOIN ext_cust_db_cust_info ci ON e.cust_id=ci.cust_id
		WHERE e.event_id=$1`
	var d domain.RiskEventDetail
	var cust, acct, rule, score, action, txnRef, summary, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx, q, eventID).Scan(
		&d.EventID, &d.BizDate, &cust, &acct, &rule, &score, &action, &txnRef, &summary, &name, &ctype)
	if err != nil {
		return domain.RiskEventDetail{}, fmt.Errorf("repo: 联邦查风控事件 %s: %w", eventID, err)
	}
	d.CustID, d.AccountNo, d.RuleID, d.RiskScore = cust.String, acct.String, rule.String, score.String
	d.ActionTaken, d.TxnRef, d.Summary, d.CustName, d.CustType = action.String, txnRef.String, summary.String, name.String, ctype.String
	return d, nil
}

// ListRules 列风控规则（静态）。
func (r *RiskRepo) ListRules(ctx context.Context) ([]domain.RiskRule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT rule_id,rule_name,rule_type,condition_json,threshold,action,status FROM risk_rule ORDER BY rule_id`)
	if err != nil {
		return nil, fmt.Errorf("repo: 列风控规则: %w", err)
	}
	defer rows.Close()
	var out []domain.RiskRule
	for rows.Next() {
		var rule domain.RiskRule
		var cond, threshold sql.NullString
		if err := rows.Scan(&rule.RuleID, &rule.RuleName, &rule.RuleType, &cond, &threshold, &rule.Action, &rule.Status); err != nil {
			return nil, fmt.Errorf("repo: 列风控规则 scan: %w", err)
		}
		rule.ConditionJSON, rule.Threshold = cond.String, threshold.String
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列风控规则: %w", err)
	}
	return out, nil
}

// ListBlacklists 按客户筛选（空则不限），分页。
func (r *RiskRepo) ListBlacklists(ctx context.Context, custID string, offset, limit int) ([]domain.Blacklist, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT list_id,cust_id,entity_type,reason,effective_biz_date,expire_date,status
		FROM blacklist WHERE ($1='' OR cust_id=$1) ORDER BY list_id LIMIT $2 OFFSET $3`, custID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列黑名单: %w", err)
	}
	defer rows.Close()
	var out []domain.Blacklist
	for rows.Next() {
		var b domain.Blacklist
		var cust, reason sql.NullString
		if err := rows.Scan(&b.ListID, &cust, &b.EntityType, &reason, &b.EffectiveBizDate, &b.ExpireDate, &b.Status); err != nil {
			return nil, fmt.Errorf("repo: 列黑名单 scan: %w", err)
		}
		b.CustID, b.Reason = cust.String, reason.String
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列黑名单: %w", err)
	}
	return out, nil
}

// scanEvent 扫描单行 risk_event（scan 函数由 QueryRow 或 Rows 注入）。
func scanEvent(scan func(dest ...any) error) (domain.RiskEvent, error) {
	var e domain.RiskEvent
	var cust, acct, rule, score, action, txnRef, summary sql.NullString
	if err := scan(&e.EventID, &e.BizDate, &cust, &acct, &rule, &score, &action, &txnRef, &summary); err != nil {
		return domain.RiskEvent{}, err
	}
	e.CustID, e.AccountNo, e.RuleID, e.RiskScore = cust.String, acct.String, rule.String, score.String
	e.ActionTaken, e.TxnRef, e.Summary = action.String, txnRef.String, summary.String
	return e, nil
}
