// Package repo 是 loan 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/loan/domain"
)

// LoanRepo loan 仓储。本库 loan_* 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type LoanRepo struct{ db *sql.DB }

// NewLoanRepo 构造 LoanRepo。
func NewLoanRepo(db *sql.DB) *LoanRepo { return &LoanRepo{db: db} }

// ListProducts 列贷款产品（静态全量）。
func (r *LoanRepo) ListProducts(ctx context.Context) ([]domain.LoanProduct, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT product_code,product_name,loan_type,rate_type,min_rate,max_rate,max_term,max_amount,status
		FROM loan_product ORDER BY product_code`)
	if err != nil {
		return nil, fmt.Errorf("repo: 列贷款产品: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanProduct
	for rows.Next() {
		var p domain.LoanProduct
		var rateType, minRate, maxRate, maxAmt, status sql.NullString
		if err := rows.Scan(&p.ProductCode, &p.ProductName, &p.LoanType, &rateType, &minRate, &maxRate, &p.MaxTerm, &maxAmt, &status); err != nil {
			return nil, fmt.Errorf("repo: 列贷款产品 scan: %w", err)
		}
		p.RateType, p.MinRate, p.MaxRate, p.Status = rateType.String, minRate.String, maxRate.String, status.String
		m, err := domain.ParseCents(maxAmt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析产品额度: %w", err)
		}
		p.MaxAmount = m
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列贷款产品: %w", err)
	}
	return out, nil
}

// ListAccounts 按产品/状态筛选借据（空则不限），分页。limit<=0 取 50。
func (r *LoanRepo) ListAccounts(ctx context.Context, productCode, status string, offset, limit int) ([]domain.LoanAccount, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code
		FROM loan_account WHERE ($1='' OR product_code=$1) AND ($2='' OR status=$2)
		ORDER BY loan_no LIMIT $3 OFFSET $4`, productCode, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列借据: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanAccount
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("repo: 列借据 scan: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列借据: %w", err)
	}
	return out, nil
}

// GetAccount 查单个借据。不存在返回包装 sql.ErrNoRows。
func (r *LoanRepo) GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error) {
	a, err := scanAccount(r.db.QueryRowContext(ctx,
		`SELECT loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code
		FROM loan_account WHERE loan_no=$1`, loanNo).Scan)
	if err != nil {
		return domain.LoanAccount{}, fmt.Errorf("repo: 查借据 %s: %w", loanNo, err)
	}
	return a, nil
}

// ListBalances 按日期范围/借据查逐日余额快照（空则不限），分页。
func (r *LoanRepo) ListBalances(ctx context.Context, from, to, loanNo string, offset, limit int) ([]domain.LoanBalance, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT loan_no,biz_date,principal_balance,interest_receivable FROM loan_balance
		WHERE (NULLIF($1,'') IS NULL OR biz_date >= NULLIF($1,'')::date)
		AND (NULLIF($2,'') IS NULL OR biz_date <= NULLIF($2,'')::date)
		AND ($3='' OR loan_no=$3)
		ORDER BY biz_date DESC, loan_no LIMIT $4 OFFSET $5`, from, to, loanNo, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列借据余额: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanBalance
	for rows.Next() {
		var b domain.LoanBalance
		var pb, ir sql.NullString
		if err := rows.Scan(&b.LoanNo, &b.BizDate, &pb, &ir); err != nil {
			return nil, fmt.Errorf("repo: 列借据余额 scan: %w", err)
		}
		var err error
		if b.PrincipalBalance, err = domain.ParseCents(pb.String); err != nil {
			return nil, fmt.Errorf("repo: 解析本金余额: %w", err)
		}
		if b.InterestReceivable, err = domain.ParseCents(ir.String); err != nil {
			return nil, fmt.Errorf("repo: 解析应收利息: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列借据余额: %w", err)
	}
	return out, nil
}

// ListOverdue 按五级分类/日期范围查逾期（空则不限），分页。
func (r *LoanRepo) ListOverdue(ctx context.Context, overdueClass, from, to string, offset, limit int) ([]domain.LoanOverdue, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT overdue_id,biz_date,loan_no,overdue_days,overdue_class,overdue_amount FROM loan_overdue
		WHERE ($1='' OR overdue_class=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date DESC, loan_no LIMIT $4 OFFSET $5`, overdueClass, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列逾期: %w", err)
	}
	defer rows.Close()
	var out []domain.LoanOverdue
	for rows.Next() {
		var o domain.LoanOverdue
		var amt sql.NullString
		if err := rows.Scan(&o.OverdueID, &o.BizDate, &o.LoanNo, &o.OverdueDays, &o.OverdueClass, &amt); err != nil {
			return nil, fmt.Errorf("repo: 列逾期 scan: %w", err)
		}
		m, err := domain.ParseCents(amt.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析逾期金额: %w", err)
		}
		o.OverdueAmount = m
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列逾期: %w", err)
	}
	return out, nil
}

// GetProfile 跨库联邦：loan_account JOIN ext_cust_db_cust_info → 借据本金/余额 + 客户姓名/类型。
func (r *LoanRepo) GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error) {
	var p domain.LoanProfile
	var principal, balance, rate, status, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT la.loan_no, la.cust_id, la.principal, la.balance, la.rate, la.status, ci.name, ci.cust_type
		FROM loan_account la
		LEFT JOIN ext_cust_db_cust_info ci ON la.cust_id=ci.cust_id
		WHERE la.loan_no=$1`, loanNo).
		Scan(&p.LoanNo, &p.CustID, &principal, &balance, &rate, &status, &name, &ctype)
	if err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 联邦查借据档案 %s: %w", loanNo, err)
	}
	if p.Principal, err = domain.ParseCents(principal.String); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 解析借据本金: %w", err)
	}
	if p.Balance, err = domain.ParseCents(balance.String); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 解析借据余额: %w", err)
	}
	p.Rate, p.Status, p.CustName, p.CustType = rate.String, status.String, name.String, ctype.String
	return p, nil
}

// scanAccount 扫描单行 loan_account（scan 函数由 QueryRow 或 Rows 注入）。
func scanAccount(scan func(dest ...any) error) (domain.LoanAccount, error) {
	var a domain.LoanAccount
	var principal, balance, rate, guarantee, branch sql.NullString
	if err := scan(&a.LoanNo, &a.CustID, &a.ProductCode, &a.Ccy, &principal, &balance, &rate,
		&a.StartBizDate, &a.MatureDate, &a.TermMonths, &a.Status, &guarantee, &branch); err != nil {
		return domain.LoanAccount{}, err
	}
	var err error
	if a.Principal, err = domain.ParseCents(principal.String); err != nil {
		return domain.LoanAccount{}, fmt.Errorf("解析本金: %w", err)
	}
	if a.Balance, err = domain.ParseCents(balance.String); err != nil {
		return domain.LoanAccount{}, fmt.Errorf("解析余额: %w", err)
	}
	a.Rate, a.GuaranteeType, a.BranchCode = rate.String, guarantee.String, branch.String
	return a, nil
}
