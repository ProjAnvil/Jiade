// Package repo is the data access layer of wealth service: this library SQL + customer HTTP API.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"bank/internal/platform/serviceclient"
	"bank/internal/wealth/domain"
)

// WealthRepo wealth storage. This library only queries wealth_*.
type WealthRepo struct {
	db       *sql.DB
	customer *serviceclient.Client
}

// NewWealthRepo Constructs WealthRepo.
func NewWealthRepo(db *sql.DB) *WealthRepo {
	return &WealthRepo{
		db:       db,
		customer: serviceclient.New(getenv("CUSTOMER_URL", "http://localhost:18081")),
	}
}

// ListProducts lists financial products (static full quantity).
func (r *WealthRepo) ListProducts(ctx context.Context) ([]domain.WealthProduct, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code,product_name,product_type,risk_level,expected_return,min_amount,term_days,start_biz_date,end_biz_date,status
		FROM wealth_product ORDER BY product_code`)
	if err != nil {
		return nil, fmt.Errorf("repo: 列理财产品: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthProduct
	for rows.Next() {
		var p domain.WealthProduct
		var risk, ret, minAmt, start, end, status sql.NullString
		if err := rows.Scan(&p.ProductCode, &p.ProductName, &p.ProductType, &risk, &ret, &minAmt, &p.TermDays, &start, &end, &status); err != nil {
			return nil, fmt.Errorf("repo: 列理财产品 scan: %w", err)
		}
		p.RiskLevel, p.ExpectedReturn, p.StartBizDate, p.EndBizDate, p.Status = risk.String, ret.String, start.String, end.String, status.String
		m, err := domain.ParseCents(minAmt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析起购金额: %w", err)
		}
		p.MinAmount = m
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列理财产品: %w", err)
	}
	return out, nil
}

// ListNav checks the daily net value by product/date range (no limit if empty; no pagination if the sequence volume is small).
func (r *WealthRepo) ListNav(ctx context.Context, productCode, from, to string) ([]domain.WealthNav, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code,biz_date,nav,accum_nav FROM wealth_nav
		WHERE ($1='' OR product_code=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date, product_code`, productCode, from, to)
	if err != nil {
		return nil, fmt.Errorf("repo: 列净值: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthNav
	for rows.Next() {
		var n domain.WealthNav
		var nav, accum sql.NullString
		if err := rows.Scan(&n.ProductCode, &n.BizDate, &nav, &accum); err != nil {
			return nil, fmt.Errorf("repo: 列净值 scan: %w", err)
		}
		n.Nav, n.AccumNav = nav.String, accum.String
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列净值: %w", err)
	}
	return out, nil
}

// ListHoldings filters holdings by customer (no limit if empty), pagination. limit<=0 takes 50.
func (r *WealthRepo) ListHoldings(ctx context.Context, custID string, offset, limit int) ([]domain.WealthHolding, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT holding_id,cust_id,account_no,product_code,ccy,share,cost,current_value,biz_date
		FROM wealth_holding WHERE ($1='' OR cust_id=$1) ORDER BY holding_id LIMIT $2 OFFSET $3`, custID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列持仓: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthHolding
	for rows.Next() {
		h, err := scanHolding(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo: 列持仓 scan: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列持仓: %w", err)
	}
	return out, nil
}

// ListOrders Check orders by customer/product/date range (no limit if empty), pagination.
func (r *WealthRepo) ListOrders(ctx context.Context, custID, productCode, from, to string, offset, limit int) ([]domain.WealthOrder, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT order_id,biz_date,cust_id,product_code,account_no,order_type,amount,share,nav,status
		FROM wealth_order WHERE ($1='' OR cust_id=$1) AND ($2='' OR product_code=$2)
		AND (NULLIF($3,'') IS NULL OR biz_date >= NULLIF($3,'')::date)
		AND (NULLIF($4,'') IS NULL OR biz_date <= NULLIF($4,'')::date)
		ORDER BY biz_date DESC, order_id LIMIT $5 OFFSET $6`, custID, productCode, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列理财订单: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthOrder
	for rows.Next() {
		var o domain.WealthOrder
		var amt, share, nav, status sql.NullString
		if err := rows.Scan(&o.OrderID, &o.BizDate, &o.CustID, &o.ProductCode, &o.AccountNo, &o.OrderType, &amt, &share, &nav, &status); err != nil {
			return nil, fmt.Errorf("repo: 列理财订单 scan: %w", err)
		}
		m, err := domain.ParseCents(amt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析订单金额: %w", err)
		}
		o.Amount, o.Share, o.Nav, o.Status = m, share.String, nav.String, status.String
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列理财订单: %w", err)
	}
	return out, nil
}

// ListIncomes Check the income by position/date range (no limit if empty), paging.
func (r *WealthRepo) ListIncomes(ctx context.Context, holdingID, from, to string, offset, limit int) ([]domain.WealthIncome, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT income_id,biz_date,holding_id,income_type,amount FROM wealth_income
		WHERE ($1='' OR holding_id=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date DESC, income_id LIMIT $4 OFFSET $5`, holdingID, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列理财收益: %w", err)
	}
	defer rows.Close()
	var out []domain.WealthIncome
	for rows.Next() {
		var inc domain.WealthIncome
		var itype, amt sql.NullString
		if err := rows.Scan(&inc.IncomeID, &inc.BizDate, &inc.HoldingID, &itype, &amt); err != nil {
			return nil, fmt.Errorf("repo: 列理财收益 scan: %w", err)
		}
		m, err := domain.ParseCents(amt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析收益金额: %w", err)
		}
		inc.IncomeType, inc.Amount = itype.String, m
		out = append(out, inc)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列理财收益: %w", err)
	}
	return out, nil
}

// GetHoldingProfile checks the library's holdings, and then calls customer to obtain the customer's name/type.
func (r *WealthRepo) GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error) {
	var p domain.WealthProfile
	var share, cv sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT holding_id,cust_id,product_code,share,current_value FROM wealth_holding WHERE holding_id=$1`, holdingID).
		Scan(&p.HoldingID, &p.CustID, &p.ProductCode, &share, &cv)
	if err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 查持仓档案 %s: %w", holdingID, err)
	}
	m, err := domain.ParseCents(cv.String)
	if err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 解析持仓市值: %w", err)
	}
	var customer struct {
		Name     string `json:"name"`
		CustType string `json:"cust_type"`
	}
	if err := r.customer.Get(ctx, "/api/v1/customers/"+serviceclient.EscapePath(p.CustID), &customer); err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 从 customer 查客户 %s: %w", p.CustID, err)
	}
	p.Share, p.CurrentValue, p.CustName, p.CustType = share.String, m, customer.Name, customer.CustType
	return p, nil
}

// scanHolding scans a single row wealth_holding (scan function is injected by QueryRow or Rows).
func scanHolding(scan func(dest ...any) error) (domain.WealthHolding, error) {
	var h domain.WealthHolding
	var share, cost, cv sql.NullString
	if err := scan(&h.HoldingID, &h.CustID, &h.AccountNo, &h.ProductCode, &h.Ccy, &share, &cost, &cv, &h.BizDate); err != nil {
		return domain.WealthHolding{}, err
	}
	var err error
	if h.Cost, err = domain.ParseCents(cost.String); err != nil {
		return domain.WealthHolding{}, fmt.Errorf("解析持仓成本: %w", err)
	}
	if h.CurrentValue, err = domain.ParseCents(cv.String); err != nil {
		return domain.WealthHolding{}, fmt.Errorf("解析持仓市值: %w", err)
	}
	h.Share = share.String
	return h, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
