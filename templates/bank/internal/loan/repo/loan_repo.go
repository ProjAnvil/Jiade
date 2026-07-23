// Package repo is the data access layer of the loan service: this library SQL + customer HTTP API.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"bank/internal/loan/domain"
	"bank/internal/platform/serviceclient"
)

// LoanRepo loan warehousing. This library only queries loan_*.
type LoanRepo struct {
	db       *sql.DB
	customer *serviceclient.Client
}

// NewLoanRepo constructs LoanRepo.
func NewLoanRepo(db *sql.DB) *LoanRepo {
	return &LoanRepo{
		db:       db,
		customer: serviceclient.New(getenv("CUSTOMER_URL", "http://localhost:18081")),
	}
}

// ListProducts lists loan products (static full quantity).
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

// ListAccounts filters IOUs by product/status (no limit if empty), pagination. limit<=0 takes 50.
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

// GetAccount checks a single IOU. Return wrapper sql.ErrNoRows does not exist.
func (r *LoanRepo) GetAccount(ctx context.Context, loanNo string) (domain.LoanAccount, error) {
	a, err := scanAccount(r.db.QueryRowContext(ctx,
		`SELECT loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code
		FROM loan_account WHERE loan_no=$1`, loanNo).Scan)
	if err != nil {
		return domain.LoanAccount{}, fmt.Errorf("repo: 查借据 %s: %w", loanNo, err)
	}
	return a, nil
}

// ListBalances Check daily balance snapshots by date range/IOU (no limit if empty), paging.
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

// ListOverdue checks overdue items by five-level classification/date range (no limit if empty), pagination.
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

// GetProfile checks the library IOU, and then calls customer to obtain the customer name/type.
func (r *LoanRepo) GetProfile(ctx context.Context, loanNo string) (domain.LoanProfile, error) {
	var p domain.LoanProfile
	var principal, balance, rate, status sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT loan_no,cust_id,principal,balance,rate,status FROM loan_account WHERE loan_no=$1`, loanNo).
		Scan(&p.LoanNo, &p.CustID, &principal, &balance, &rate, &status)
	if err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 查借据档案 %s: %w", loanNo, err)
	}
	if p.Principal, err = domain.ParseCents(principal.String); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 解析借据本金: %w", err)
	}
	if p.Balance, err = domain.ParseCents(balance.String); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 解析借据余额: %w", err)
	}
	var customer struct {
		Name     string `json:"name"`
		CustType string `json:"cust_type"`
	}
	if err := r.customer.Get(ctx, "/api/v1/customers/"+serviceclient.EscapePath(p.CustID), &customer); err != nil {
		return domain.LoanProfile{}, fmt.Errorf("repo: 从 customer 查客户 %s: %w", p.CustID, err)
	}
	p.Rate, p.Status, p.CustName, p.CustType = rate.String, status.String, customer.Name, customer.CustType
	return p, nil
}

// scanAccount scans a single row loan_account (scan function is injected by QueryRow or Rows).
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

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
