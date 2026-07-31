package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// Funds-hold errors.
var (
	// ErrInsufficientAvailableBalance — available (ledger balance - active holds) is less than the requested hold amount.
	ErrInsufficientAvailableBalance = fmt.Errorf("可用余额不足")
	// ErrHoldNotFound — no hold matches the given id or idempotency key.
	ErrHoldNotFound = fmt.Errorf("hold: 不存在")
	// ErrNonPositiveHoldAmount — hold amount must be positive.
	ErrNonPositiveHoldAmount = fmt.Errorf("hold: 金额必须为正")
)

// HoldStore is the persistence interface for funds holds (repo implements).
// All methods accept pg.DBTX so the caller can run balance-lock + hold-insert
// in one transaction.
type HoldStore interface {
	// LockLatestBalance ensures the current biz_date balance row exists for
	// accountNo (inheriting from the latest historical row if needed), then
	// locks it (FOR UPDATE) and returns it. This is the same row that
	// transfers/deposits lock, so holds serialize against all ledger writes
	// on the account.
	LockLatestBalance(ctx context.Context, q pg.DBTX, accountNo string) (domain.Balance, error)
	// LockActiveHolds locks (FOR UPDATE) and returns all active holds for accountNo.
	LockActiveHolds(ctx context.Context, q pg.DBTX, accountNo string) ([]domain.Hold, error)
	// GetHoldByIdempotencyKey returns the hold for key, or an error wrapping ErrHoldNotFound.
	GetHoldByIdempotencyKey(ctx context.Context, q pg.DBTX, key string) (domain.Hold, error)
	// InsertHold persists a new hold. The unique idempotency_key constraint is
	// the last line of defence against concurrent duplicates.
	InsertHold(ctx context.Context, q pg.DBTX, h domain.Hold) error
	// LockHoldByID locks (FOR UPDATE) and returns the hold for holdID.
	LockHoldByID(ctx context.Context, q pg.DBTX, holdID string) (domain.Hold, error)
	// SetHoldStatus updates the status (and updated_at) of a hold.
	SetHoldStatus(ctx context.Context, q pg.DBTX, holdID string, status domain.HoldStatus) error
}

// HoldService orchestrates funds-hold placement and release.
// available = ledger balance - active holds, always computed under the
// balance-row lock inside a transaction.
type HoldService struct {
	db    *sql.DB
	store HoldStore
}

// NewHoldService constructs a funds-hold service. db is the transaction
// boundary (nil for unit tests — fn runs with q=nil against a fake store).
func NewHoldService(db *sql.DB, store HoldStore) *HoldService {
	return &HoldService{db: db, store: store}
}

// PlaceHold reserves available funds on an account. Idempotent on
// IdempotencyKey: a duplicate request returns the existing hold unchanged.
// Rejects if available (ledger balance - active holds) is insufficient.
func (s *HoldService) PlaceHold(ctx context.Context, in domain.PlaceHoldInput) (domain.Hold, error) {
	if in.Amount <= 0 {
		return domain.Hold{}, ErrNonPositiveHoldAmount
	}

	var result domain.Hold
	run := pg.RunInTx
	if s.db == nil {
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err := run(ctx, s.db, func(q pg.DBTX) error {
		// Fast path: return existing hold if this idempotency key is already
		// persisted (avoids the balance lock for late-arriving retries).
		if existing, err := s.store.GetHoldByIdempotencyKey(ctx, q, in.IdempotencyKey); err == nil {
			result = existing
			return nil
		} else if !errors.Is(err, ErrHoldNotFound) {
			return err
		}

		// Lock the account's current balance row. This serializes PlaceHold
		// against transfers/deposits/withdrawals on the same account.
		bal, err := s.store.LockLatestBalance(ctx, q, in.AccountNo)
		if err != nil {
			return err
		}
		// Lock active holds so ReleaseHold/Capture cannot change them concurrently.
		activeHolds, err := s.store.LockActiveHolds(ctx, q, in.AccountNo)
		if err != nil {
			return err
		}

		// Re-check idempotency inside the lock: a concurrent same-key PlaceHold
		// may have committed while we waited for the balance lock.
		if existing, err := s.store.GetHoldByIdempotencyKey(ctx, q, in.IdempotencyKey); err == nil {
			result = existing
			return nil
		} else if !errors.Is(err, ErrHoldNotFound) {
			return err
		}

		// available = ledger balance - active holds (integer minor units).
		held := domain.Money(0)
		for _, h := range activeHolds {
			held = held.Add(h.Amount)
		}
		available := bal.Balance.Sub(held)
		if in.Amount > available {
			return ErrInsufficientAvailableBalance
		}

		hold := domain.Hold{
			HoldID:         domain.NewHoldID(),
			IdempotencyKey: in.IdempotencyKey,
			AccountNo:      in.AccountNo,
			Amount:         in.Amount,
			Ccy:            in.Ccy,
			WorkflowID:     in.WorkflowID,
			Status:         domain.HoldStatusActive,
			ExpiresAt:      in.ExpiresAt,
		}
		if err := s.store.InsertHold(ctx, q, hold); err != nil {
			return err
		}
		result = hold
		return nil
	})
	if err != nil {
		return domain.Hold{}, err
	}
	return result, nil
}

// ReleaseHold releases an active hold. Idempotent: re-releasing an already-
// released hold returns it without error and without a redundant write.
// A captured hold cannot be released (returns domain.ErrHoldCaptured).
// The idempotencyKey parameter is accepted for saga command-dedup at the
// consumer layer; at the service level, release idempotency is status-based.
func (s *HoldService) ReleaseHold(ctx context.Context, holdID, idempotencyKey string) (domain.Hold, error) {
	var result domain.Hold
	run := pg.RunInTx
	if s.db == nil {
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err := run(ctx, s.db, func(q pg.DBTX) error {
		hold, err := s.store.LockHoldByID(ctx, q, holdID)
		if err != nil {
			return err
		}
		prevStatus := hold.Status
		if err := hold.Release(); err != nil {
			return err // domain.ErrHoldCaptured for captured holds
		}
		// Persist only when the status actually changed (skip redundant write
		// for the idempotent re-release path).
		if hold.Status != prevStatus {
			if err := s.store.SetHoldStatus(ctx, q, holdID, hold.Status); err != nil {
				return err
			}
		}
		result = hold
		return nil
	})
	if err != nil {
		return domain.Hold{}, err
	}
	return result, nil
}
