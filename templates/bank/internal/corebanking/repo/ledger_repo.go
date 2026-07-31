package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
)

// Compile-time assertions: LedgerRepo satisfies the write interfaces it serves.
var (
	_ service.LedgerStore   = (*LedgerRepo)(nil)
	_ service.TransferStore = (*LedgerRepo)(nil)
)

// LedgerRepo General Ledger/Double Entry Warehousing. Implements service.LedgerStore(write) + read-only GetGL.
type LedgerRepo struct {
	db *sql.DB
}

func NewLedgerRepo(db *sql.DB) *LedgerRepo { return &LedgerRepo{db: db} }

// InsertTxns implements service.LedgerStore.InsertTxns. Generated when TxnID is empty and backfilled into txns (visible to the caller).
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

// ApplyBalanceDeltas implements service.LedgerStore.ApplyBalanceDeltas(ON CONFLICT accumulation).
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

// UpsertGL implements service.LedgerStore.UpsertGL (general ledger accumulation).
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

// GetGL checks the general ledger of a biz_date (used by API /ledger). For read-only methods, still use r.db.
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

// EnsureBalanceRow locks the account's balance row on biz_date on the current day (FOR UPDATE). If there is no row on that day, it will start from the latest historical row.
// Inherit balance/available/frozen to the current day (solve cross-day inheritance: ApplyBalanceDeltas only accumulates but does not inherit).
// Returns the basis of today's available balance (= today's balance after inheritance). The caller must subsequently call ApplyBalanceDeltas within the same transaction.
//
// Concurrency safety: "Build the same day" inherited across days is INSERT... ON CONFLICT DO NOTHING - the first transaction is successful,
// The construction bank of concurrent transactions is swallowed by ON CONFLICT (duplicate-key is not reported). After building the bank, FOR UPDATE for "Same day trip",
// Make concurrent transfers serialize on the current day's row (accumulate to a consistent critical section with ON CONFLICT of ApplyBalanceDeltas).
// This is the key fix for B-3 acceptance of "concurrent A→B/B→A without deadlock and correct balance": the original implementation only locks historical rows,
// Both transactions' FOR UPDATE see latestDate<bizDate, and their respective INSERTs cause unique conflicts.
func (r *LedgerRepo) EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error) {
	// 1. If the current row does not exist, inherit the CCB from the latest historical row (ON CONFLICT swallows the duplicate-key of the concurrent CCB).
	if _, err := q.ExecContext(ctx, `
		INSERT INTO account_balance (account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
		SELECT $1, $2, balance, available_balance, frozen_amount, COALESCE(NULLIF($3,''), subject_code)
		FROM account_balance WHERE account_no=$1
		ORDER BY biz_date DESC LIMIT 1
		ON CONFLICT (account_no,biz_date) DO NOTHING`,
		accountNo, bizDate, subjectCode); err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 继承余额到 %s 失败: %w", bizDate, err)
	}

	// 2. Lock the current day's row and read the authoritative baseline balance. Concurrent transfers are made serially (same day).
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

// GetTxnsByVoucher checks all transactions under a certain voucher (in order of entry).
func (r *LedgerRepo) GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error) {
	rows, err := q.QueryContext(ctx, `SELECT txn_id,biz_date::text,txn_ts::text,account_no,dc_flag,amount::text,ccy,
		subject_code,COALESCE(opp_account,''),COALESCE(ref_txn_id,''),COALESCE(channel,''),COALESCE(summary,''),txn_status
		FROM acct_txn WHERE voucher_no=$1 ORDER BY txn_ts`, voucherNo)
	if err != nil {
		return nil, fmt.Errorf("repo: 查凭证流水: %w", err)
	}
	defer rows.Close()
	return scanTxnRows(rows, voucherNo)
}

// LockTxnsByVoucher is the same as GetTxnsByVoucher, but with the addition of FOR UPDATE - special for reversal,
// Lock all pipeline lines of this voucher within the transaction and serialize concurrent revisions of the same voucher (see service.Reverse).
func (r *LedgerRepo) LockTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error) {
	rows, err := q.QueryContext(ctx, `SELECT txn_id,biz_date::text,txn_ts::text,account_no,dc_flag,amount::text,ccy,
		subject_code,COALESCE(opp_account,''),COALESCE(ref_txn_id,''),COALESCE(channel,''),COALESCE(summary,''),txn_status
		FROM acct_txn WHERE voucher_no=$1 ORDER BY txn_ts FOR UPDATE`, voucherNo)
	if err != nil {
		return nil, fmt.Errorf("repo: 锁凭证流水: %w", err)
	}
	defer rows.Close()
	return scanTxnRows(rows, voucherNo)
}

// scanTxnRows scans the row set common to GetTxnsByVoucher/LockTxnsByVoucher.
func scanTxnRows(rows *sql.Rows, voucherNo string) ([]domain.Txn, error) {
	var out []domain.Txn
	for rows.Next() {
		var t domain.Txn
		var amountStr, status string
		if err := rows.Scan(&t.TxnID, &t.BizDate, &t.TxnTs, &t.AccountNo, &t.DCFlag, &amountStr,
			&t.Ccy, &t.SubjectCode, &t.OppAccount, &t.RefTxnID, &t.Channel, &t.Summary, &status); err != nil {
			return nil, fmt.Errorf("repo: 扫描凭证流水: %w", err)
		}
		var err error
		if t.Amount, err = domain.ParseCents(amountStr); err != nil {
			return nil, err
		}
		t.TxnStatus = domain.TxnStatus(status)
		t.VoucherNo = voucherNo
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasReversal checks whether there is already a flow with refTxnID as ref_txn_id (the criterion for red reverse entries to be dropped into the database).
// After the red punch is serialized through LockTxnsByVoucher, use this method to determine whether "the first correction has been committed."
func (r *LedgerRepo) HasReversal(ctx context.Context, q pg.DBTX, refTxnID string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM acct_txn WHERE ref_txn_id=$1)`, refTxnID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("repo: 查红冲存在性: %w", err)
	}
	return exists, nil
}

// UpdateTxnStatus changes the status of all transactions under a certain voucher to status (for blue flushing).
//
// Concurrency safety: Add txn_status='normal' guard to the WHERE clause - turn normal→reversed into one
// Atomic UPDATE. Only one of the two concurrent blue flushes can change to row (RowsAffected>0), and the other row 0 → return
// sql.ErrNoRows(service side errors.Is → ErrAlreadyReversed).
// The row lock (LockTxnsByVoucher) ensures that two blue rushes enter this statement serially, and the second one is already reversed when entering.
func (r *LedgerRepo) UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error {
	res, err := q.ExecContext(ctx, `UPDATE acct_txn SET txn_status=$2 WHERE voucher_no=$1 AND txn_status='normal'`,
		voucherNo, string(status))
	if err != nil {
		return fmt.Errorf("repo: 改流水状态: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Distinguish between two types of 0 lines: the voucher has no flow vs. has been corrected concurrently. The latter is the criterion for correcting competition;
		// The service uses errors.Is(err, sql.ErrNoRows) to identify and then convert it to ErrAlreadyReversed.
		// No flow (voucher does not exist) will be intercepted by the preceding LockTxnsByVoucher → len==0 → ErrVoucherNotFound.
		// So when you get here, it must be "This voucher has a running water but it has been reversed."
		return fmt.Errorf("repo: 凭证 %s 流水已非 normal（并发冲正或重复冲正）: %w", voucherNo, sql.ErrNoRows)
	}
	return nil
}

// SetTxnSummary changes the summary of all transactions under a certain voucher to the incoming value (Record is called after Post, and Post does not fill in summary).
func (r *LedgerRepo) SetTxnSummary(ctx context.Context, q pg.DBTX, voucherNo, summary string) error {
	_, err := q.ExecContext(ctx, `UPDATE acct_txn SET summary=$2 WHERE voucher_no=$1`,
		voucherNo, summary)
	if err != nil {
		return fmt.Errorf("repo: 更新流水摘要: %w", err)
	}
	return nil
}

// GetBizDate reads sys_param.biz_date (B-2 connection: accounting biz_date is taken from sys_param, not time.Now).
func (r *LedgerRepo) GetBizDate(ctx context.Context) (string, error) {
	var v string
	err := r.db.QueryRowContext(ctx, "SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&v)
	if err != nil {
		return "", fmt.Errorf("repo: 读 sys_param.biz_date: %w", err)
	}
	return v, nil
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

// --- TransferStore: held-transfer idempotency + reversal tracking + outbox ---

// GetHeldTransferByKey returns the voucher_no recorded for an idempotency key.
// sql.ErrNoRows is translated to service.ErrHeldTransferNotFound.
func (r *LedgerRepo) GetHeldTransferByKey(ctx context.Context, q pg.DBTX, key string) (string, error) {
	var voucherNo string
	err := q.QueryRowContext(ctx,
		`SELECT voucher_no FROM held_transfer WHERE idempotency_key=$1`, key).Scan(&voucherNo)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("repo: 按 key 查 held_transfer %q: %w", key, service.ErrHeldTransferNotFound)
		}
		return "", fmt.Errorf("repo: 按 key 查 held_transfer %q: %w", key, err)
	}
	return voucherNo, nil
}

// InsertHeldTransfer records the idempotency-key → voucher mapping. The unique
// PK on idempotency_key is the last-line guard against concurrent duplicates.
func (r *LedgerRepo) InsertHeldTransfer(ctx context.Context, q pg.DBTX, key, voucherNo, holdID string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO held_transfer (idempotency_key, voucher_no, hold_id) VALUES ($1, $2, $3)`,
		key, voucherNo, holdID)
	if err != nil {
		return fmt.Errorf("repo: 插入 held_transfer: %w", err)
	}
	return nil
}

// HasReversalForVoucher checks whether a reversal row exists for voucherNo.
// The hard duplicate-reversal guard is the PK on reverses_voucher_no; this
// soft check is a friendly early-out inside the row lock.
func (r *LedgerRepo) HasReversalForVoucher(ctx context.Context, q pg.DBTX, voucherNo string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM voucher_reversal WHERE reverses_voucher_no=$1)`, voucherNo).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("repo: 查冲正存在性: %w", err)
	}
	return exists, nil
}

// InsertReversal records that reversalVoucherNo reverses originalVoucherNo.
// The PK on reverses_voucher_no makes a concurrent duplicate INSERT fail and
// the enclosing transaction rolls back.
func (r *LedgerRepo) InsertReversal(ctx context.Context, q pg.DBTX, reversesVoucherNo, reversalVoucherNo string) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO voucher_reversal (reverses_voucher_no, reversal_voucher_no) VALUES ($1, $2)`,
		reversesVoucherNo, reversalVoucherNo)
	if err != nil {
		return fmt.Errorf("repo: 插入 voucher_reversal: %w", err)
	}
	return nil
}

// AppendOutbox inserts a messaging envelope into outbox_message in the current
// transaction. Mirrors risk.appendOutbox and the workflow engine AppendOutbox
// so the shared outbox_message table is populated identically.
func (r *LedgerRepo) AppendOutbox(ctx context.Context, q pg.DBTX, env messaging.Envelope, routingKey string) error {
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("repo: marshal outbox envelope %s: %w", env.MessageID, err)
	}
	_, err = q.ExecContext(ctx, `
		INSERT INTO outbox_message
		  (message_id, message_type, schema_version, routing_key, envelope,
		   attempts, created_at)
		VALUES ($1, $2, $3, $4, $5, 0, CURRENT_TIMESTAMP)`,
		env.MessageID, env.MessageType, env.SchemaVersion, routingKey, []byte(body),
	)
	if err != nil {
		return fmt.Errorf("repo: insert outbox_message %s: %w", env.MessageID, err)
	}
	return nil
}
