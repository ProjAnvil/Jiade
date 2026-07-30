package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
	"bank/internal/platform/pg"
)

// HoldRepo funds-hold persistence. Implements service.HoldStore.
type HoldRepo struct {
	db *sql.DB
}

func NewHoldRepo(db *sql.DB) *HoldRepo { return &HoldRepo{db: db} }

// Compile-time assertion: HoldRepo satisfies HoldStore.
var _ service.HoldStore = (*HoldRepo)(nil)

// holdColumns is the canonical column list + casts for scanning a hold row.
const holdColumns = `hold_id, idempotency_key, account_no, amount::text, ccy, workflow_id, status,
	expires_at, created_at, updated_at`

// LockLatestBalance ensures the current biz_date balance row exists for
// accountNo (inheriting from the latest historical row), then locks it
// (FOR UPDATE) and returns it. Mirrors LedgerRepo.EnsureBalanceRow so that
// holds serialize against transfers on the same balance row.
func (r *HoldRepo) LockLatestBalance(ctx context.Context, q pg.DBTX, accountNo string) (domain.Balance, error) {
	// Read sys_param.biz_date (the authoritative accounting date, not time.Now).
	var bizDate string
	if err := q.QueryRowContext(ctx,
		"SELECT param_value FROM sys_param WHERE param_key='biz_date'").Scan(&bizDate); err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return domain.Balance{}, fmt.Errorf("repo: sys_param.biz_date 未设置")
	}

	// Inherit the latest historical balance into the current biz_date if the
	// row does not yet exist (ON CONFLICT swallows the concurrent duplicate).
	if _, err := q.ExecContext(ctx, `
		INSERT INTO account_balance (account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
		SELECT $1, $2, balance, available_balance, frozen_amount, subject_code
		FROM account_balance WHERE account_no=$1
		ORDER BY biz_date DESC LIMIT 1
		ON CONFLICT (account_no,biz_date) DO NOTHING`,
		accountNo, bizDate); err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 继承余额到 %s 失败: %w", bizDate, err)
	}

	// Lock the current day's row.
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
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Balance{}, fmt.Errorf("repo: 账户 %s 无余额记录: %w", accountNo, service.ErrHoldNotFound)
		}
		return domain.Balance{}, fmt.Errorf("repo: 锁余额 %s: %w", accountNo, err)
	}
	b.AccountNo = accountNo
	b.BizDate = bizDate
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

// LockActiveHolds locks (FOR UPDATE) and returns all active holds for accountNo.
func (r *HoldRepo) LockActiveHolds(ctx context.Context, q pg.DBTX, accountNo string) ([]domain.Hold, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT `+holdColumns+` FROM funds_hold WHERE account_no=$1 AND status='active' FOR UPDATE`,
		accountNo)
	if err != nil {
		return nil, fmt.Errorf("repo: 锁活跃 hold: %w", err)
	}
	defer rows.Close()
	return scanHoldRows(rows)
}

// GetHoldByIdempotencyKey returns the hold for key, or a wrapped ErrHoldNotFound.
func (r *HoldRepo) GetHoldByIdempotencyKey(ctx context.Context, q pg.DBTX, key string) (domain.Hold, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+holdColumns+` FROM funds_hold WHERE idempotency_key=$1`, key)
	h, err := scanHoldRow(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Hold{}, fmt.Errorf("repo: 按 key 查 hold %q: %w", key, service.ErrHoldNotFound)
		}
		return domain.Hold{}, fmt.Errorf("repo: 按 key 查 hold %q: %w", key, err)
	}
	return h, nil
}

// InsertHold persists a new hold. expires_at is NULL when ExpiresAt is zero.
func (r *HoldRepo) InsertHold(ctx context.Context, q pg.DBTX, h domain.Hold) error {
	var expiresAt any
	if !h.ExpiresAt.IsZero() {
		expiresAt = h.ExpiresAt
	}
	_, err := q.ExecContext(ctx, `INSERT INTO funds_hold
		(hold_id,idempotency_key,account_no,amount,ccy,workflow_id,status,expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		h.HoldID, h.IdempotencyKey, h.AccountNo, h.Amount.String(), h.Ccy,
		h.WorkflowID, string(h.Status), expiresAt)
	if err != nil {
		return fmt.Errorf("repo: 插入 hold: %w", err)
	}
	return nil
}

// LockHoldByID locks (FOR UPDATE) and returns the hold for holdID.
func (r *HoldRepo) LockHoldByID(ctx context.Context, q pg.DBTX, holdID string) (domain.Hold, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+holdColumns+` FROM funds_hold WHERE hold_id=$1 FOR UPDATE`, holdID)
	h, err := scanHoldRow(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Hold{}, fmt.Errorf("repo: 按 ID 查 hold %s: %w", holdID, service.ErrHoldNotFound)
		}
		return domain.Hold{}, fmt.Errorf("repo: 锁 hold %s: %w", holdID, err)
	}
	return h, nil
}

// SetHoldStatus updates status and updated_at for a hold.
func (r *HoldRepo) SetHoldStatus(ctx context.Context, q pg.DBTX, holdID string, status domain.HoldStatus) error {
	_, err := q.ExecContext(ctx,
		`UPDATE funds_hold SET status=$2, updated_at=now() WHERE hold_id=$1`,
		holdID, string(status))
	if err != nil {
		return fmt.Errorf("repo: 更新 hold 状态: %w", err)
	}
	return nil
}

// scanFn is the common Scan signature shared by *sql.Row and *sql.Rows.
type scanFn func(dest ...any) error

// scanHoldRow decodes a single hold row using the provided Scan function.
func scanHoldRow(scan scanFn) (domain.Hold, error) {
	var (
		h         domain.Hold
		amountStr string
		statusStr string
		expiresAt sql.NullTime
		createdAt time.Time
		updatedAt time.Time
	)
	if err := scan(&h.HoldID, &h.IdempotencyKey, &h.AccountNo, &amountStr, &h.Ccy,
		&h.WorkflowID, &statusStr, &expiresAt, &createdAt, &updatedAt); err != nil {
		return domain.Hold{}, err
	}
	amt, err := domain.ParseCents(amountStr)
	if err != nil {
		return domain.Hold{}, err
	}
	h.Amount = amt
	h.Status = domain.HoldStatus(statusStr)
	if expiresAt.Valid {
		h.ExpiresAt = expiresAt.Time
	}
	h.CreatedAt = createdAt
	h.UpdatedAt = updatedAt
	return h, nil
}

// scanHoldRows iterates a rows set into a hold slice.
func scanHoldRows(rows *sql.Rows) ([]domain.Hold, error) {
	var out []domain.Hold
	for rows.Next() {
		var (
			h         domain.Hold
			amountStr string
			statusStr string
			expiresAt sql.NullTime
			createdAt time.Time
			updatedAt time.Time
		)
		if err := rows.Scan(&h.HoldID, &h.IdempotencyKey, &h.AccountNo, &amountStr, &h.Ccy,
			&h.WorkflowID, &statusStr, &expiresAt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("repo: 扫描 hold: %w", err)
		}
		amt, err := domain.ParseCents(amountStr)
		if err != nil {
			return nil, err
		}
		h.Amount = amt
		h.Status = domain.HoldStatus(statusStr)
		if expiresAt.Valid {
			h.ExpiresAt = expiresAt.Time
		}
		h.CreatedAt = createdAt
		h.UpdatedAt = updatedAt
		out = append(out, h)
	}
	return out, rows.Err()
}
