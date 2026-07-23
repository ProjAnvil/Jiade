// Package repo is the data access layer of risk service: this library SQL + customer HTTP API.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"bank/internal/platform/serviceclient"
	"bank/internal/risk/domain"
)

// RiskRepo risk warehousing. This library only queries risk_event/risk_rule/blacklist.
type RiskRepo struct {
	db       *sql.DB
	customer *serviceclient.Client
}

// NewRiskRepo constructs RiskRepo.
func NewRiskRepo(db *sql.DB) *RiskRepo {
	return &RiskRepo{
		db:       db,
		customer: serviceclient.New(getenv("CUSTOMER_URL", "http://localhost:18081")),
	}
}

// ListEvents filter by date/rule/action (no limit if empty), pagination.
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

// GetEvent checks the library event, and then calls customer to obtain the customer name/type.
func (r *RiskRepo) GetEvent(ctx context.Context, eventID string) (domain.RiskEventDetail, error) {
	q := `SELECT event_id,biz_date,cust_id,account_no,rule_id,risk_score,action_taken,txn_ref,summary
		FROM risk_event WHERE event_id=$1`
	var d domain.RiskEventDetail
	var cust, acct, rule, score, action, txnRef, summary sql.NullString
	err := r.db.QueryRowContext(ctx, q, eventID).Scan(
		&d.EventID, &d.BizDate, &cust, &acct, &rule, &score, &action, &txnRef, &summary)
	if err != nil {
		return domain.RiskEventDetail{}, fmt.Errorf("repo: 查风控事件 %s: %w", eventID, err)
	}
	d.CustID, d.AccountNo, d.RuleID, d.RiskScore = cust.String, acct.String, rule.String, score.String
	d.ActionTaken, d.TxnRef, d.Summary = action.String, txnRef.String, summary.String
	if d.CustID != "" {
		var customer struct {
			Name     string `json:"name"`
			CustType string `json:"cust_type"`
		}
		if err := r.customer.Get(ctx, "/api/v1/customers/"+serviceclient.EscapePath(d.CustID), &customer); err != nil {
			return domain.RiskEventDetail{}, fmt.Errorf("repo: 从 customer 查客户 %s: %w", d.CustID, err)
		}
		d.CustName, d.CustType = customer.Name, customer.CustType
	}
	return d, nil
}

// ListRules lists risk control rules (static).
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

// ListBlacklists Filter by customer (no limit if empty), paging.
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

// scanEvent scans a single row risk_event (scan function is injected by QueryRow or Rows).
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

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
