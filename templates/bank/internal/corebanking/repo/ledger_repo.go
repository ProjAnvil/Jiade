package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"bank/internal/corebanking/domain"
)

// LedgerRepo 总账/复式记账仓储。实现 service.LedgerStore（写）+ 只读 GetGL。
type LedgerRepo struct {
	db *sql.DB
}

func NewLedgerRepo(db *sql.DB) *LedgerRepo { return &LedgerRepo{db: db} }

// InsertTxns 实现 service.LedgerStore.InsertTxns。
func (r *LedgerRepo) InsertTxns(ctx context.Context, txns []domain.Txn) error {
	for _, t := range txns {
		id := t.TxnID
		if id == "" {
			id = newTxnID()
		}
		_, err := r.db.ExecContext(ctx, `INSERT INTO acct_txn
			(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			id, t.BizDate, t.AccountNo, string(t.DCFlag), t.Amount.String(), t.Ccy, t.SubjectCode,
			nullable(t.OppAccount), nullable(t.RefTxnID), nullable(t.Channel), nullable(t.Summary))
		if err != nil {
			return fmt.Errorf("repo: 插入流水: %w", err)
		}
	}
	return nil
}

// ApplyBalanceDeltas 实现 service.LedgerStore.ApplyBalanceDeltas（ON CONFLICT 累加，无需读旧值）。
func (r *LedgerRepo) ApplyBalanceDeltas(ctx context.Context, bizDate string, deltas []domain.BalanceDelta) error {
	for _, d := range deltas {
		_, err := r.db.ExecContext(ctx, `INSERT INTO account_balance
			(account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
			VALUES ($1,$2,$3,$3,0,$4)
			ON CONFLICT (account_no,biz_date) DO UPDATE
			SET balance=account_balance.balance+EXCLUDED.balance,
			    available_balance=account_balance.available_balance+EXCLUDED.available_balance`,
			d.AccountNo, bizDate, d.Delta.String(), d.SubjectCode)
		if err != nil {
			return fmt.Errorf("repo: 累加余额: %w", err)
		}
	}
	return nil
}

// UpsertGL 实现 service.LedgerStore.UpsertGL（总账累加）。
func (r *LedgerRepo) UpsertGL(ctx context.Context, gl domain.GLBalance) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO gl_balance
		(subject_code,biz_date,dc_balance,cc_balance,ccy)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (subject_code,biz_date,ccy) DO UPDATE
		SET dc_balance=gl_balance.dc_balance+EXCLUDED.dc_balance,
		    cc_balance=gl_balance.cc_balance+EXCLUDED.cc_balance`,
		gl.SubjectCode, gl.BizDate, gl.DCBalance.String(), gl.CCBalance.String(), gl.Ccy)
	if err != nil {
		return fmt.Errorf("repo: 更新总账: %w", err)
	}
	return nil
}

// GetGL 查某 biz_date 的总账（API /ledger 用）。
func (r *LedgerRepo) GetGL(ctx context.Context, bizDate string) ([]domain.GLBalance, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT subject_code,biz_date::text,dc_balance::text,cc_balance::text,ccy
		 FROM gl_balance WHERE biz_date=$1 ORDER BY subject_code`, bizDate)
	if err != nil {
		return nil, fmt.Errorf("repo: 查总账: %w", err)
	}
	defer rows.Close()
	var out []domain.GLBalance
	for rows.Next() {
		var g domain.GLBalance
		var dcStr, ccStr string
		if err := rows.Scan(&g.SubjectCode, &g.BizDate, &dcStr, &ccStr, &g.Ccy); err != nil {
			return nil, fmt.Errorf("repo: 扫描总账: %w", err)
		}
		if g.DCBalance, err = domain.ParseCents(dcStr); err != nil {
			return nil, err
		}
		if g.CCBalance, err = domain.ParseCents(ccStr); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newTxnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "T" + hex.EncodeToString(b)
}
