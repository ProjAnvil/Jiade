// Package repo is the data access layer of the payment service: SQL of this library + HTTP API of other microservices.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"bank/internal/payment/domain"
	"bank/internal/platform/serviceclient"
)

// PaymentRepo payment repository. This library only queries transfer_txn/merchant.
type PaymentRepo struct {
	db       *sql.DB
	core     *serviceclient.Client
	customer *serviceclient.Client
}

// NewPaymentRepo constructs PaymentRepo.
func NewPaymentRepo(db *sql.DB) *PaymentRepo {
	return &PaymentRepo{
		db:       db,
		core:     serviceclient.New(getenv("CORE_BANKING_URL", "http://localhost:18080")),
		customer: serviceclient.New(getenv("CUSTOMER_URL", "http://localhost:18081")),
	}
}

// ListTransfers filters transfers by account/date (no limit if empty), pagination. When limit<=0, take 50.
func (r *PaymentRepo) ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary
		FROM transfer_txn WHERE ($1='' OR out_account=$1 OR in_account=$1)
		AND (NULLIF($2,'') IS NULL OR biz_date >= NULLIF($2,'')::date)
		AND (NULLIF($3,'') IS NULL OR biz_date <= NULLIF($3,'')::date)
		ORDER BY biz_date DESC, txn_id LIMIT $4 OFFSET $5`
	rows, err := r.db.QueryContext(ctx, q, accountNo, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列转账: %w", err)
	}
	defer rows.Close()
	out, err := scanTransfers(rows)
	if err != nil {
		return nil, fmt.Errorf("repo: 列转账: %w", err)
	}
	return out, nil
}

// GetTransfer checks a single transfer. There is no return wrapped sql.ErrNoRows.
func (r *PaymentRepo) GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary
		FROM transfer_txn WHERE txn_id=$1`, txnID)
	t, err := scanTransfer(row.Scan)
	if err != nil {
		return domain.Transfer{}, fmt.Errorf("repo: 查转账 %s: %w", txnID, err)
	}
	return t, nil
}

// GetTransferParties checks the bank transfer, and then calls core-banking/customer to obtain the names of both parties' customers.
func (r *PaymentRepo) GetTransferParties(ctx context.Context, txnID string) (domain.TransferParty, error) {
	var p domain.TransferParty
	var amtStr string
	err := r.db.QueryRowContext(ctx,
		`SELECT txn_id,amount,ccy,biz_date,out_account,in_account FROM transfer_txn WHERE txn_id=$1`,
		txnID).Scan(&p.TxnID, &amtStr, &p.Ccy, &p.BizDate, &p.OutAccount, &p.InAccount)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 查转账双方 %s: %w", txnID, err)
	}
	amt, err := domain.ParseCents(amtStr)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 联邦查转账双方 %s 解析金额: %w", txnID, err)
	}
	p.Amount = amt
	p.OutCustName, err = r.customerNameForAccount(ctx, p.OutAccount)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 查转出方 %s: %w", p.OutAccount, err)
	}
	p.InCustName, err = r.customerNameForAccount(ctx, p.InAccount)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 查转入方 %s: %w", p.InAccount, err)
	}
	return p, nil
}

func (r *PaymentRepo) customerNameForAccount(ctx context.Context, accountNo string) (string, error) {
	var account struct {
		CustID string `json:"cust_id"`
	}
	if err := r.core.Get(ctx, "/api/v1/accounts/"+serviceclient.EscapePath(accountNo), &account); err != nil {
		return "", err
	}
	var customer struct {
		Name string `json:"name"`
	}
	if err := r.customer.Get(ctx, "/api/v1/customers/"+serviceclient.EscapePath(account.CustID), &customer); err != nil {
		return "", err
	}
	return customer.Name, nil
}

// GetMerchant Check merchants. There is no return wrapped sql.ErrNoRows.
func (r *PaymentRepo) GetMerchant(ctx context.Context, merchantID string) (domain.Merchant, error) {
	var m domain.Merchant
	err := r.db.QueryRowContext(ctx, `SELECT merchant_id,merchant_name,mcc,region,status,create_biz_date
		FROM merchant WHERE merchant_id=$1`, merchantID).
		Scan(&m.MerchantID, &m.MerchantName, &m.MCC, &m.Region, &m.Status, &m.CreateBizDate)
	if err != nil {
		return domain.Merchant{}, fmt.Errorf("repo: 查商户 %s: %w", merchantID, err)
	}
	return m, nil
}

// scanTransfers scans transfer lines in batches (DRY: ListTransfers multiplexing).
func scanTransfers(rows *sql.Rows) ([]domain.Transfer, error) {
	var out []domain.Transfer
	for rows.Next() {
		t, err := scanTransfer(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("扫描转账行: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("遍历转账行: %w", err)
	}
	return out, nil
}

// scanTransfer single row scan (scan function is injected by QueryRow or Rows, DRY: Get/List multiplexing).
func scanTransfer(scan func(dest ...any) error) (domain.Transfer, error) {
	var t domain.Transfer
	var amount, fee string
	var channel, counter, summary sql.NullString
	if err := scan(&t.TxnID, &t.BizDate, &t.OutAccount, &t.InAccount, &amount, &t.Ccy,
		&fee, &channel, &counter, &t.Status, &summary); err != nil {
		return domain.Transfer{}, fmt.Errorf("扫描转账字段: %w", err)
	}
	amt, err := domain.ParseCents(amount)
	if err != nil {
		return domain.Transfer{}, fmt.Errorf("解析金额 %q: %w", amount, err)
	}
	f, err := domain.ParseCents(fee)
	if err != nil {
		return domain.Transfer{}, fmt.Errorf("解析手续费 %q: %w", fee, err)
	}
	t.Amount, t.Fee = amt, f
	t.Channel, t.CounterBank, t.Summary = channel.String, counter.String, summary.String
	return t, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
