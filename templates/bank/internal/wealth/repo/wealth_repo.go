// Package repo 是 wealth 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/wealth/domain"
)

// WealthRepo wealth 仓储。本库 wealth_* 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type WealthRepo struct{ db *sql.DB }

// NewWealthRepo 构造 WealthRepo。
func NewWealthRepo(db *sql.DB) *WealthRepo { return &WealthRepo{db: db} }

// ListProducts 列理财产品（静态全量）。
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

// ListNav 按产品/日期范围查每日净值（空则不限；序列量小不分页）。
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

// ListHoldings 按客户筛选持仓（空则不限），分页。limit<=0 取 50。
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

// ListOrders 按客户/产品/日期范围查订单（空则不限），分页。
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

// ListIncomes 按持仓/日期范围查收益（空则不限），分页。
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

// GetHoldingProfile 跨库联邦：wealth_holding JOIN ext_cust_db_cust_info → 持仓份额/市值 + 客户姓名/类型。
func (r *WealthRepo) GetHoldingProfile(ctx context.Context, holdingID string) (domain.WealthProfile, error) {
	var p domain.WealthProfile
	var share, cv, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT h.holding_id, h.cust_id, h.product_code, h.share, h.current_value, ci.name, ci.cust_type
		FROM wealth_holding h
		LEFT JOIN ext_cust_db_cust_info ci ON h.cust_id=ci.cust_id
		WHERE h.holding_id=$1`, holdingID).
		Scan(&p.HoldingID, &p.CustID, &p.ProductCode, &share, &cv, &name, &ctype)
	if err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 联邦查持仓档案 %s: %w", holdingID, err)
	}
	m, err := domain.ParseCents(cv.String)
	if err != nil {
		return domain.WealthProfile{}, fmt.Errorf("repo: 解析持仓市值: %w", err)
	}
	p.Share, p.CurrentValue, p.CustName, p.CustType = share.String, m, name.String, ctype.String
	return p, nil
}

// scanHolding 扫描单行 wealth_holding（scan 函数由 QueryRow 或 Rows 注入）。
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
