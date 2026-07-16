// Package repo 是 payment 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/payment/domain"
)

// PaymentRepo payment 仓储。本库 transfer_txn / merchant 查询，并经 FDW 跨库 JOIN
// ext_core_db_demand_account + ext_cust_db_cust_info（由 platform/fdw 在 seed 阶段于 pay_db 建立）。
type PaymentRepo struct{ db *sql.DB }

// NewPaymentRepo 构造 PaymentRepo。
func NewPaymentRepo(db *sql.DB) *PaymentRepo { return &PaymentRepo{db: db} }

// ListTransfers 按账户/日期筛选转账（空则不限），分页。limit<=0 时取 50。
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

// GetTransfer 查单笔转账。不存在返回包装的 sql.ErrNoRows。
func (r *PaymentRepo) GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary
		FROM transfer_txn WHERE txn_id=$1`, txnID)
	t, err := scanTransfer(row.Scan)
	if err != nil {
		return domain.Transfer{}, fmt.Errorf("repo: 查转账 %s: %w", txnID, err)
	}
	return t, nil
}

// GetTransferParties 跨库联邦：transfer_txn JOIN ext_core_db_demand_account(×2) JOIN ext_cust_db_cust_info(×2)。
// 返回转账双方账户 + 户主客户姓名。
func (r *PaymentRepo) GetTransferParties(ctx context.Context, txnID string) (domain.TransferParty, error) {
	q := `SELECT t.txn_id, t.amount, t.ccy, t.biz_date,
			t.out_account, oc.name, t.in_account, ic.name
		FROM transfer_txn t
		LEFT JOIN ext_core_db_demand_account od ON t.out_account=od.account_no
		LEFT JOIN ext_cust_db_cust_info oc ON od.cust_id=oc.cust_id
		LEFT JOIN ext_core_db_demand_account id ON t.in_account=id.account_no
		LEFT JOIN ext_cust_db_cust_info ic ON id.cust_id=ic.cust_id
		WHERE t.txn_id=$1`
	var p domain.TransferParty
	var outName, inName sql.NullString
	var amtStr string
	err := r.db.QueryRowContext(ctx, q, txnID).Scan(
		&p.TxnID, &amtStr, &p.Ccy, &p.BizDate,
		&p.OutAccount, &outName, &p.InAccount, &inName)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 联邦查转账双方 %s: %w", txnID, err)
	}
	amt, err := domain.ParseCents(amtStr)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 联邦查转账双方 %s 解析金额: %w", txnID, err)
	}
	p.Amount = amt
	p.OutCustName, p.InCustName = outName.String, inName.String
	return p, nil
}

// GetMerchant 查商户。不存在返回包装的 sql.ErrNoRows。
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

// scanTransfers 批量扫描转账行（DRY：ListTransfers 复用）。
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

// scanTransfer 单行扫描（scan 函数由 QueryRow 或 Rows 注入，DRY：Get/List 复用）。
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
