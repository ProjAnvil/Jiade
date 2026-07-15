// Package repo 是 customer 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/customer/domain"
)

// CustomerRepo 客户仓储。本库 cust_info / cust_account_rel 查询，并经 FDW 跨库 JOIN core_db。
type CustomerRepo struct{ db *sql.DB }

// NewCustomerRepo 构造 CustomerRepo。
func NewCustomerRepo(db *sql.DB) *CustomerRepo { return &CustomerRepo{db: db} }

// GetCustomer 查单个客户。不存在返回包装的 sql.ErrNoRows。
func (r *CustomerRepo) GetCustomer(ctx context.Context, custID string) (domain.Customer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT cust_id,cust_type,name,cert_type,cert_no,gender,birthday,
		nationality,risk_level,kyc_status,create_biz_date FROM cust_info WHERE cust_id=$1`, custID)
	var c domain.Customer
	var cType, gender, birthday sql.NullString
	err := row.Scan(&c.CustID, &cType, &c.Name, &c.CertType, &c.CertNo, &gender, &birthday,
		&c.Nationality, &c.RiskLevel, &c.KYCStatus, &c.CreateBizDate)
	if err != nil {
		return domain.Customer{}, fmt.Errorf("repo: 查客户 %s: %w", custID, err)
	}
	c.CustType = domain.CustType(cType.String)
	c.Gender, c.Birthday = gender.String, birthday.String
	return c, nil
}

// ListCustomers 按客户类型/kyc 筛选（空则不限），分页。limit<=0 时取 50。
func (r *CustomerRepo) ListCustomers(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT cust_id,cust_type,name,cert_type,cert_no,gender,birthday,nationality,risk_level,kyc_status,create_biz_date
		FROM cust_info WHERE ($1='' OR cust_type=$1) AND ($2='' OR kyc_status=$2)
		ORDER BY cust_id LIMIT $3 OFFSET $4`
	rows, err := r.db.QueryContext(ctx, q, custType, kycStatus, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列客户: %w", err)
	}
	defer rows.Close()
	var out []domain.Customer
	for rows.Next() {
		var c domain.Customer
		var cType, gender, birthday sql.NullString
		if err := rows.Scan(&c.CustID, &cType, &c.Name, &c.CertType, &c.CertNo, &gender, &birthday,
			&c.Nationality, &c.RiskLevel, &c.KYCStatus, &c.CreateBizDate); err != nil {
			return nil, err
		}
		c.CustType = domain.CustType(cType.String)
		c.Gender, c.Birthday = gender.String, birthday.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCustAccounts 跨库联邦查询：cust_account_rel JOIN ext_core_db_demand_account（FDW）。
// ext_core_db_demand_account 由 platform/fdw 在 seed 阶段于 cust_db 建立，映射 core_db.demand_account。
func (r *CustomerRepo) GetCustAccounts(ctx context.Context, custID string) ([]domain.CustAccount, error) {
	q := `SELECT a.account_no, a.ccy, a.acct_status, a.open_biz_date, a.branch_code, rel.role
		FROM cust_account_rel rel
		JOIN ext_core_db_demand_account a ON rel.account_no = a.account_no
		WHERE rel.cust_id=$1 ORDER BY a.account_no`
	rows, err := r.db.QueryContext(ctx, q, custID)
	if err != nil {
		return nil, fmt.Errorf("repo: 联邦查客户账户 %s: %w", custID, err)
	}
	defer rows.Close()
	var out []domain.CustAccount
	for rows.Next() {
		var a domain.CustAccount
		var branch sql.NullString
		if err := rows.Scan(&a.AccountNo, &a.Ccy, &a.Status, &a.OpenBizDate, &branch, &a.Role); err != nil {
			return nil, err
		}
		a.BranchCode = branch.String
		out = append(out, a)
	}
	return out, rows.Err()
}
