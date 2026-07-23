// Package repo is the core-banking warehousing layer: pgx raw SQL is dropped into the library.
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/corebanking/domain"
)

// AccountRepo Account repository. Implement service.AccountStore (write) + read-only query.
type AccountRepo struct {
	db *sql.DB
}

func NewAccountRepo(db *sql.DB) *AccountRepo { return &AccountRepo{db: db} }

// InsertDemand implements service.AccountStore.InsertDemand.
func (r *AccountRepo) InsertDemand(ctx context.Context, a domain.DemandAccount) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO demand_account
		(account_no,cust_id,ccy,acct_status,open_biz_date,branch_code,product_code,subject_code)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		a.AccountNo, a.CustID, a.Ccy, string(a.Status), a.OpenBizDate,
		a.BranchCode, a.ProductCode, a.SubjectCode)
	if err != nil {
		return fmt.Errorf("repo: 插入活期账户: %w", err)
	}
	return nil
}

// InsertFixed implements service.AccountStore.InsertFixed.
func (r *AccountRepo) InsertFixed(ctx context.Context, a domain.FixedAccount) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO fixed_account
		(account_no,cust_id,ccy,principal,rate,term_months,start_biz_date,mature_date,acct_status,subject_code)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.AccountNo, a.CustID, a.Ccy, a.Principal.String(), a.Rate, a.TermMonths,
		a.StartBizDate, a.MatureDate, string(a.Status), a.SubjectCode)
	if err != nil {
		return fmt.Errorf("repo: 插入定期账户: %w", err)
	}
	return nil
}

// SetDemandStatus implements service.AccountStore.SetDemandStatus.
func (r *AccountRepo) SetDemandStatus(ctx context.Context, accountNo string, status domain.AccountStatus) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE demand_account SET acct_status=$2 WHERE account_no=$1`, accountNo, string(status))
	if err != nil {
		return fmt.Errorf("repo: 更新账户状态: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("repo: 账户 %s 不存在", accountNo)
	}
	return nil
}

// GetDemand checks current accounts. Returns wrapped sql.ErrNoRows when not present.
func (r *AccountRepo) GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error) {
	row := r.db.QueryRowContext(ctx, `SELECT account_no,cust_id,ccy,acct_status,open_biz_date,
		branch_code,product_code,subject_code FROM demand_account WHERE account_no=$1`, accountNo)
	var a domain.DemandAccount
	var status string
	err := row.Scan(&a.AccountNo, &a.CustID, &a.Ccy, &status, &a.OpenBizDate,
		&a.BranchCode, &a.ProductCode, &a.SubjectCode)
	if err != nil {
		return domain.DemandAccount{}, fmt.Errorf("repo: 查活期账户 %s: %w", accountNo, err)
	}
	a.Status = domain.AccountStatus(status)
	return a, nil
}

// GetFixed checks term accounts.
func (r *AccountRepo) GetFixed(ctx context.Context, accountNo string) (domain.FixedAccount, error) {
	row := r.db.QueryRowContext(ctx, `SELECT account_no,cust_id,ccy,principal,rate,term_months,
		start_biz_date,mature_date,acct_status,subject_code FROM fixed_account WHERE account_no=$1`, accountNo)
	var (
		a            domain.FixedAccount
		status       string
		principalStr string
		rateStr      string
	)
	err := row.Scan(&a.AccountNo, &a.CustID, &a.Ccy, &principalStr, &rateStr, &a.TermMonths,
		&a.StartBizDate, &a.MatureDate, &status, &a.SubjectCode)
	if err != nil {
		return domain.FixedAccount{}, fmt.Errorf("repo: 查定期账户 %s: %w", accountNo, err)
	}
	p, err := domain.ParseCents(principalStr)
	if err != nil {
		return domain.FixedAccount{}, err
	}
	a.Principal = p
	a.Rate = rateStr
	a.Status = domain.AccountStatus(status)
	return a, nil
}
