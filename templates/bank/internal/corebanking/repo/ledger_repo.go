package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// LedgerRepo 总账/复式记账仓储。实现 service.LedgerStore（写）+ 只读 GetGL。
type LedgerRepo struct {
	db *sql.DB
}

func NewLedgerRepo(db *sql.DB) *LedgerRepo { return &LedgerRepo{db: db} }

// InsertTxns 实现 service.LedgerStore.InsertTxns。TxnID 为空时生成并回填到 txns（调用方可见）。
func (r *LedgerRepo) InsertTxns(ctx context.Context, q pg.DBTX, txns []domain.Txn) error {
	for i := range txns {
		if txns[i].TxnID == "" {
			txns[i].TxnID = newTxnID()
		}
		_, err := q.ExecContext(ctx, `INSERT INTO acct_txn
			(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary,voucher_no,txn_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			txns[i].TxnID, txns[i].BizDate, txns[i].AccountNo, string(txns[i].DCFlag), txns[i].Amount.String(),
			txns[i].Ccy, txns[i].SubjectCode, nullable(txns[i].OppAccount), nullable(txns[i].RefTxnID),
			nullable(txns[i].Channel), nullable(txns[i].Summary), txns[i].VoucherNo, string(txns[i].TxnStatus))
		if err != nil {
			return fmt.Errorf("repo: 插入流水: %w", err)
		}
	}
	return nil
}

// ApplyBalanceDeltas 实现 service.LedgerStore.ApplyBalanceDeltas（ON CONFLICT 累加）。
func (r *LedgerRepo) ApplyBalanceDeltas(ctx context.Context, q pg.DBTX, bizDate string, deltas []domain.BalanceDelta) error {
	for _, d := range deltas {
		_, err := q.ExecContext(ctx, `INSERT INTO account_balance
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
func (r *LedgerRepo) UpsertGL(ctx context.Context, q pg.DBTX, gl domain.GLBalance) error {
	_, err := q.ExecContext(ctx, `INSERT INTO gl_balance
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

// GetGL 查某 biz_date 的总账（API /ledger 用）。只读方法，仍用 r.db。
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

// EnsureBalanceRow 锁定账户当天 biz_date 的余额行（FOR UPDATE），若当天无行则从最新历史行
// 继承 balance/available/frozen 到当天（解决跨日继承：ApplyBalanceDeltas 只累加不继承）。
// 返回当天可用余额基准（= 继承后的当天余额）。调用方须在同一事务内随后调用 ApplyBalanceDeltas。
//
// 并发安全：跨日继承的「建当天行」是 INSERT ... ON CONFLICT DO NOTHING —— 首个事务建行成功，
// 并发事务的建行被 ON CONFLICT 吞掉（不报 duplicate-key）。建行后再对「当天行」FOR UPDATE，
// 使并发转账在当天行上串行（与 ApplyBalanceDeltas 的 ON CONFLICT 累加形成一致的临界区）。
// 这是 B-3 验收「并发 A→B/B→A 不死锁且余额正确」的关键修复：原实现仅锁历史行，
// 两个事务的 FOR UPDATE 都看到 latestDate<bizDate，各自 INSERT 导致 unique 冲突。
func (r *LedgerRepo) EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error) {
	// 1. 若当天行不存在，从最新历史行继承建行（ON CONFLICT 吞掉并发建行的 duplicate-key）。
	if _, err := q.ExecContext(ctx, `
		INSERT INTO account_balance (account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
		SELECT $1, $2, balance, available_balance, frozen_amount, COALESCE(NULLIF($3,''), subject_code)
		FROM account_balance WHERE account_no=$1
		ORDER BY biz_date DESC LIMIT 1
		ON CONFLICT (account_no,biz_date) DO NOTHING`,
		accountNo, bizDate, subjectCode); err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 继承余额到 %s 失败: %w", bizDate, err)
	}

	// 2. 锁定当天行并读取权威基准余额。并发转账在此串行（同一当天行）。
	var (
		b                           domain.Balance
		balStr, availStr, frozenStr string
	)
	err := q.QueryRowContext(ctx, `
		SELECT balance::text, available_balance::text, frozen_amount::text
		FROM account_balance WHERE account_no=$1 AND biz_date=$2 FOR UPDATE`,
		accountNo, bizDate).
		Scan(&balStr, &availStr, &frozenStr)
	if err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 锁当天余额失败 %s %s: %w", accountNo, bizDate, err)
	}
	b.AccountNo = accountNo
	b.BizDate = bizDate
	b.SubjectCode = subjectCode
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

// GetTxnsByVoucher 查某凭证下的所有流水（按入账顺序）。
func (r *LedgerRepo) GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error) {
	rows, err := q.QueryContext(ctx, `SELECT txn_id,biz_date::text,txn_ts::text,account_no,dc_flag,amount::text,ccy,
		subject_code,COALESCE(opp_account,''),COALESCE(ref_txn_id,''),COALESCE(channel,''),COALESCE(summary,''),txn_status
		FROM acct_txn WHERE voucher_no=$1 ORDER BY txn_ts`, voucherNo)
	if err != nil {
		return nil, fmt.Errorf("repo: 查凭证流水: %w", err)
	}
	defer rows.Close()
	var out []domain.Txn
	for rows.Next() {
		var t domain.Txn
		var amountStr, status string
		if err := rows.Scan(&t.TxnID, &t.BizDate, &t.TxnTs, &t.AccountNo, &t.DCFlag, &amountStr,
			&t.Ccy, &t.SubjectCode, &t.OppAccount, &t.RefTxnID, &t.Channel, &t.Summary, &status); err != nil {
			return nil, fmt.Errorf("repo: 扫描凭证流水: %w", err)
		}
		if t.Amount, err = domain.ParseCents(amountStr); err != nil {
			return nil, err
		}
		t.TxnStatus = domain.TxnStatus(status)
		t.VoucherNo = voucherNo
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTxnStatus 把某凭证下所有流水状态改为 status（蓝冲用）。
func (r *LedgerRepo) UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error {
	res, err := q.ExecContext(ctx, `UPDATE acct_txn SET txn_status=$2 WHERE voucher_no=$1`,
		voucherNo, string(status))
	if err != nil {
		return fmt.Errorf("repo: 改流水状态: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("repo: 凭证 %s 无流水", voucherNo)
	}
	return nil
}

// SetTxnSummary 把某凭证下所有流水的 summary 改为传入值（Record 在 Post 后调用，Post 不填 summary）。
func (r *LedgerRepo) SetTxnSummary(ctx context.Context, q pg.DBTX, voucherNo, summary string) error {
	_, err := q.ExecContext(ctx, `UPDATE acct_txn SET summary=$2 WHERE voucher_no=$1`,
		voucherNo, summary)
	if err != nil {
		return fmt.Errorf("repo: 更新流水摘要: %w", err)
	}
	return nil
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
