package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/corebanking/domain"
)

// TxnRepo flow/balance storage. Implement service.TxnStore.
type TxnRepo struct {
	db *sql.DB
}

func NewTxnRepo(db *sql.DB) *TxnRepo { return &TxnRepo{db: db} }

// ListTxns implements service.TxnStore.ListTxns (from/to is YYYY-MM-DD, empty means no limit).
func (r *TxnRepo) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	q := `SELECT txn_id,biz_date::text,txn_ts::text,account_no,dc_flag,amount::text,ccy,subject_code,
		COALESCE(opp_account,''),COALESCE(ref_txn_id,''),COALESCE(channel,''),COALESCE(summary,'')
		FROM acct_txn WHERE 1=1`
	args := []any{}
	if accountNo != "" {
		args = append(args, accountNo)
		q += fmt.Sprintf(" AND account_no=$%d", len(args))
	}
	if from != "" {
		args = append(args, from)
		q += fmt.Sprintf(" AND biz_date>=$%d", len(args))
	}
	if to != "" {
		args = append(args, to)
		q += fmt.Sprintf(" AND biz_date<=$%d", len(args))
	}
	q += " ORDER BY txn_ts DESC LIMIT 200"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: 查流水: %w", err)
	}
	defer rows.Close()
	var out []domain.Txn
	for rows.Next() {
		var t domain.Txn
		var amountStr string
		if err := rows.Scan(&t.TxnID, &t.BizDate, &t.TxnTs, &t.AccountNo, &t.DCFlag,
			&amountStr, &t.Ccy, &t.SubjectCode, &t.OppAccount, &t.RefTxnID,
			&t.Channel, &t.Summary); err != nil {
			return nil, fmt.Errorf("repo: 扫描流水: %w", err)
		}
		amt, err := domain.ParseCents(amountStr)
		if err != nil {
			return nil, err
		}
		t.Amount = amt
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetLatestBalance implements service.TxnStore.GetLatestBalance (gets the latest biz_date snapshot).
func (r *TxnRepo) GetLatestBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	row := r.db.QueryRowContext(ctx, `SELECT account_no,biz_date::text,balance::text,available_balance::text,
		frozen_amount::text,subject_code FROM account_balance
		WHERE account_no=$1 ORDER BY biz_date DESC LIMIT 1`, accountNo)
	var (
		b         domain.Balance
		balStr    string
		availStr  string
		frozenStr string
	)
	err := row.Scan(&b.AccountNo, &b.BizDate, &balStr, &availStr, &frozenStr, &b.SubjectCode)
	if err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 查余额 %s: %w", accountNo, err)
	}
	if b.Balance, err = domain.ParseCents(balStr); err != nil {
		return domain.Balance{}, err
	}
	if b.AvailableBalance, err = domain.ParseCents(availStr); err != nil {
		return domain.Balance{}, err
	}
	if b.FrozenAmount, err = domain.ParseCents(frozenStr); err != nil {
		return domain.Balance{}, err
	}
	return b, nil
}
