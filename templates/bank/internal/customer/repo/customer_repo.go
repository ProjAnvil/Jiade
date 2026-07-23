// Package repo is the data access layer of the customer service: this library SQL + core-banking HTTP API.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"bank/internal/customer/domain"
	"bank/internal/platform/serviceclient"
)

// CustomerRepo Customer repository. This library only queries cust_info / cust_account_rel.
type CustomerRepo struct {
	db   *sql.DB
	core *serviceclient.Client
}

// NewCustomerRepo Constructs CustomerRepo.
func NewCustomerRepo(db *sql.DB) *CustomerRepo {
	return &CustomerRepo{db: db, core: serviceclient.New(getenv("CORE_BANKING_URL", "http://localhost:18080"))}
}

// GetCustomer checks a single customer. There is no return wrapped sql.ErrNoRows.
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

// ListCustomers Filter by customer type/kyc (no limit if empty), paging. When limit<=0, take 50.
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
			return nil, fmt.Errorf("repo: 列客户 scan: %w", err)
		}
		c.CustType = domain.CustType(cType.String)
		c.Gender, c.Birthday = gender.String, birthday.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列客户: %w", err)
	}
	return out, nil
}

// GetCustAccounts first checks the database relationship, and then obtains the account information through the core-banking API.
func (r *CustomerRepo) GetCustAccounts(ctx context.Context, custID string) ([]domain.CustAccount, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT account_no,role FROM cust_account_rel WHERE cust_id=$1 ORDER BY account_no`, custID)
	if err != nil {
		return nil, fmt.Errorf("repo: 查客户账户关系 %s: %w", custID, err)
	}
	defer rows.Close()
	var out []domain.CustAccount
	for rows.Next() {
		var accountNo, role string
		if err := rows.Scan(&accountNo, &role); err != nil {
			return nil, fmt.Errorf("repo: 查客户账户关系 %s scan: %w", custID, err)
		}
		var account struct {
			AccountNo   string `json:"account_no"`
			Ccy         string `json:"ccy"`
			Status      string `json:"status"`
			OpenBizDate string `json:"open_biz_date"`
			BranchCode  string `json:"branch_code"`
		}
		if err := r.core.Get(ctx, "/api/v1/accounts/"+serviceclient.EscapePath(accountNo), &account); err != nil {
			return nil, fmt.Errorf("repo: 从 core-banking 查账户 %s: %w", accountNo, err)
		}
		out = append(out, domain.CustAccount{
			AccountNo: account.AccountNo, Ccy: account.Ccy, Status: account.Status,
			OpenBizDate: account.OpenBizDate, BranchCode: account.BranchCode, Role: role,
		})
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 查客户账户关系 %s: %w", custID, err)
	}
	return out, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
