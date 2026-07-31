package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
)

// Outbox message types and routing keys for held-transfer events.
const (
	// EventTransferPosted is emitted when a held transfer is committed.
	EventTransferPosted = "core.transfer-posted.v1"
	// EventTransferReversed is emitted when a held transfer is reversed.
	EventTransferReversed = "core.transfer-reversed.v1"
	// RouteTransferPosted is the broker routing key for posted events.
	RouteTransferPosted = "core.transfer.posted"
	// RouteTransferReversed is the broker routing key for reversed events.
	RouteTransferReversed = "core.transfer.reversed"
)

// Held-transfer / reversal errors.
var (
	// ErrHeldTransferNotFound — no held transfer matches the idempotency key.
	ErrHeldTransferNotFound = fmt.Errorf("transfer: 幂等记录不存在")
	// ErrHoldNotActive — the hold is not in the active state.
	ErrHoldNotActive = fmt.Errorf("transfer: hold 非 active 状态")
	// ErrHoldAmountMismatch — requested amount differs from the hold amount.
	ErrHoldAmountMismatch = fmt.Errorf("transfer: 金额与 hold 不一致")
	// ErrHoldCcyMismatch — requested currency differs from the hold currency.
	ErrHoldCcyMismatch = fmt.Errorf("transfer: 币种与 hold 不一致")
	// ErrHoldAccountMismatch — from-account differs from the hold's account.
	ErrHoldAccountMismatch = fmt.Errorf("transfer: 账户与 hold 不一致")
	// ErrOriginalVoucherNotFound — the voucher to reverse has no entries.
	ErrOriginalVoucherNotFound = fmt.Errorf("transfer: 原凭证不存在")
	// ErrVoucherAlreadyReversed — a reversal already exists for the voucher.
	ErrVoucherAlreadyReversed = fmt.Errorf("transfer: 凭证已冲正")
)

// SagaRouting carries the command envelope's saga-routing fields through the
// service so the service-emitted result envelope (core.transfer-posted.v1 /
// core.transfer-reversed.v1) carries the same correlation/causation context
// that the consumer stamps on its own events via makeResultEnvelope. The saga
// engine's ApplyResult correlates results on workflow_id + action_name +
// command_id; without these the outbox relay rejects the envelope
// ("correlation_id is required") and the action stalls in waiting_result.
// The consumer populates these fields from the inbound command envelope.
type SagaRouting struct {
	WorkflowID       string
	ActionName       string
	CommandID        string
	CorrelationID    string
	CommandMessageID string // the command envelope's MessageID, echoed as CausationID
}

// PostHeldTransfer command: captures an active hold and posts balanced
// debit/credit entries in one core-banking transaction. Amount, currency and
// from-account must exactly match the hold. Idempotent on IdempotencyKey.
type PostHeldTransfer struct {
	IdempotencyKey string
	HoldID         string
	FromAccount    string       // debit account (must match hold's account)
	ToAccount      string       // credit account (counterparty)
	Amount         domain.Money // must match hold's amount exactly
	Ccy            string       // must match hold's currency exactly
	Summary        string
	SagaRouting    SagaRouting
}

// ReverseTransfer command: creates a red-reversal voucher whose two entries
// invert the original debit and credit. The original voucher's status and
// entries remain UNCHANGED. A unique reverses_voucher_no prevents a duplicate
// reversal of the same original.
type ReverseTransfer struct {
	IdempotencyKey    string
	OriginalVoucherNo string
	Summary           string
	SagaRouting       SagaRouting
}

// TransferStore is the persistence port for held-transfer posting and
// reversal tracking. Implemented by repo.LedgerRepo; all methods accept
// pg.DBTX so they run inside the caller's transaction.
type TransferStore interface {
	// GetHeldTransferByKey returns the voucher_no of a previously posted held
	// transfer. Returns a wrapped ErrHeldTransferNotFound when the key is unknown.
	GetHeldTransferByKey(ctx context.Context, q pg.DBTX, key string) (string, error)
	// InsertHeldTransfer persists the idempotency-key → voucher mapping. The
	// unique PK on idempotency_key is the last-line defence against duplicates.
	InsertHeldTransfer(ctx context.Context, q pg.DBTX, key, voucherNo, holdID string) error
	// HasReversalForVoucher reports whether a reversal already exists for
	// voucherNo. The hard guarantee is the unique PK on voucher_reversal.
	HasReversalForVoucher(ctx context.Context, q pg.DBTX, voucherNo string) (bool, error)
	// InsertReversal records that reversalVoucher reverses originalVoucher.
	// Violating the unique PK on reverses_voucher_no returns a duplicate error.
	InsertReversal(ctx context.Context, q pg.DBTX, reversesVoucherNo, reversalVoucherNo string) error
	// AppendOutbox inserts a messaging envelope into outbox_message in the
	// current transaction. On failure the entire transaction rolls back.
	AppendOutbox(ctx context.Context, q pg.DBTX, env messaging.Envelope, routingKey string) error
}

// HeldTransferService orchestrates held-ledger transfers and red reversals.
// Each operation runs in a single core-banking transaction (pg.RunInTx) so
// that hold capture, entry posting, idempotency tracking and the outbox
// envelope commit atomically — or roll back together on any failure.
type HeldTransferService struct {
	db          *sql.DB
	holds       HoldStore
	accounts    AccountReader
	ledger      *LedgerService
	ledgerStore LedgerStore
	transfers   TransferStore
}

// NewHeldTransferService constructs a HeldTransferService. db is the
// transaction boundary (nil for unit tests — fn runs with q=nil against
// fakes). holds locks/captures holds; accounts resolves account metadata;
// ledger posts balanced entries; ledgerStore manages balance rows and
// voucher queries; transfers tracks idempotency, reversals and outbox.
func NewHeldTransferService(
	db *sql.DB,
	holds HoldStore,
	accounts AccountReader,
	ledger *LedgerService,
	ledgerStore LedgerStore,
	transfers TransferStore,
) *HeldTransferService {
	return &HeldTransferService{
		db:          db,
		holds:       holds,
		accounts:    accounts,
		ledger:      ledger,
		ledgerStore: ledgerStore,
		transfers:   transfers,
	}
}

// PostHeldTransfer captures an active hold and posts balanced debit/credit
// entries in one transaction. Orchestration (brief Step 3):
//
//	SELECT hold FOR UPDATE → validate active + exact amount/ccy/account →
//	insert debit and credit entries → update balances → mark hold captured →
//	insert core.transfer-posted.v1 Outbox → commit.
//
// Idempotent on IdempotencyKey: a duplicate request returns the previously
// committed voucher unchanged.
func (s *HeldTransferService) PostHeldTransfer(ctx context.Context, in PostHeldTransfer) (domain.Booking, error) {
	bizDate, err := s.ledgerStore.GetBizDate(ctx)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("held transfer: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return domain.Booking{}, fmt.Errorf("held transfer: sys_param.biz_date 未设置")
	}

	var booking domain.Booking

	run := pg.RunInTx
	if s.db == nil {
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err = run(ctx, s.db, func(q pg.DBTX) error {
		// Fast path: duplicate idempotency key → return the existing voucher.
		if existingVoucher, err := s.transfers.GetHeldTransferByKey(ctx, q, in.IdempotencyKey); err == nil {
			txns, gErr := s.ledgerStore.GetTxnsByVoucher(ctx, q, existingVoucher)
			if gErr != nil {
				return fmt.Errorf("held transfer: 读现有凭证流水: %w", gErr)
			}
			booking = domain.Booking{VoucherNo: existingVoucher, BizDate: bizDate, Txns: txns}
			return nil
		} else if !errors.Is(err, ErrHeldTransferNotFound) {
			return err
		}

		// Step 1: SELECT hold FOR UPDATE.
		hold, err := s.holds.LockHoldByID(ctx, q, in.HoldID)
		if err != nil {
			return err
		}

		// Step 2: validate active hold + exact amount/currency/account.
		if hold.Status != domain.HoldStatusActive {
			return fmt.Errorf("%w: hold %s 当前 %q", ErrHoldNotActive, in.HoldID, hold.Status)
		}
		if hold.Amount != in.Amount {
			return fmt.Errorf("%w: 请求 %s, hold %s", ErrHoldAmountMismatch, in.Amount, hold.Amount)
		}
		if hold.Ccy != in.Ccy {
			return fmt.Errorf("%w: 请求 %s, hold %s", ErrHoldCcyMismatch, in.Ccy, hold.Ccy)
		}
		if hold.AccountNo != in.FromAccount {
			return fmt.Errorf("%w: 请求 %s, hold %s", ErrHoldAccountMismatch, in.FromAccount, hold.AccountNo)
		}

		// Resolve both accounts (from = held account, to = counterparty).
		fromAcct, err := s.accounts.GetDemand(ctx, in.FromAccount)
		if err != nil {
			return ErrAccountNotFound
		}
		if fromAcct.Status != domain.AccountStatusActive {
			return ErrAccountNotActive
		}
		if in.Ccy != "" && in.Ccy != fromAcct.Ccy {
			return ErrCcyMismatch
		}
		toAcct, err := s.accounts.GetDemand(ctx, in.ToAccount)
		if err != nil {
			return ErrAccountNotFound
		}
		if toAcct.Status != domain.AccountStatusActive {
			return ErrAccountNotActive
		}
		if toAcct.Ccy != in.Ccy {
			return ErrCcyMismatch
		}

		// Build balanced entries: debit from-account / credit to-account.
		entries, err := BuildHeldTransferEntries(fromAcct, toAcct, in.Amount)
		if err != nil {
			return err
		}

		// Lock balance rows (ascending account_no to prevent AB-BA deadlock),
		// inheriting cross-day balances into bizDate.
		lockAccounts := []string{in.FromAccount, in.ToAccount}
		sort.Strings(lockAccounts)
		for _, no := range lockAccounts {
			subject := fromAcct.SubjectCode
			if no == in.ToAccount {
				subject = toAcct.SubjectCode
			}
			if _, err := s.ledgerStore.EnsureBalanceRow(ctx, q, no, bizDate, subject); err != nil {
				return err
			}
		}

		// Steps 3-4: insert debit/credit entries + update balances + GL.
		voucherNo := domain.NewVoucherNo(bizDate)
		txns, err := s.ledger.Post(ctx, q, entries, bizDate, in.Ccy, voucherNo, "")
		if err != nil {
			return err
		}

		if in.Summary != "" {
			if err := s.ledgerStore.SetTxnSummary(ctx, q, voucherNo, in.Summary); err != nil {
				return err
			}
			for i := range txns {
				txns[i].Summary = in.Summary
			}
		}

		// Step 5: mark hold captured.
		if err := s.holds.SetHoldStatus(ctx, q, in.HoldID, domain.HoldStatusCaptured); err != nil {
			return err
		}

		// Record idempotency mapping (unique PK guards concurrent duplicates).
		if err := s.transfers.InsertHeldTransfer(ctx, q, in.IdempotencyKey, voucherNo, in.HoldID); err != nil {
			return err
		}

		// Step 6: insert core.transfer-posted.v1 Outbox — failure rolls back
		// the entire transaction (entries, balances, hold capture, mapping).
		payload, err := buildTransferPostedPayload(voucherNo, in)
		if err != nil {
			return err
		}
		env := makeTransferResultEnvelope(EventTransferPosted, in.IdempotencyKey, in.SagaRouting, payload)
		if err := s.transfers.AppendOutbox(ctx, q, env, RouteTransferPosted); err != nil {
			return err
		}

		booking = domain.Booking{VoucherNo: voucherNo, BizDate: bizDate, Txns: txns}
		return nil
	})
	if err != nil {
		return domain.Booking{}, err
	}
	return booking, nil
}

// ReverseTransfer creates a red-reversal voucher whose two entries invert the
// original debit and credit. The original voucher's status and entries remain
// UNCHANGED (red reversal appends, never edits). A unique reverses_voucher_no
// constraint prevents a duplicate reversal of the same original.
//
// Orchestration (brief Step 4), all in one transaction:
//
//	LockTxnsByVoucher (FOR UPDATE) → HasReversal soft check → build reverse
//	entries → Post new voucher → InsertReversal (unique PK) → insert
//	core.transfer-reversed.v1 Outbox → commit.
func (s *HeldTransferService) ReverseTransfer(ctx context.Context, in ReverseTransfer) (domain.Booking, error) {
	bizDate, err := s.ledgerStore.GetBizDate(ctx)
	if err != nil {
		return domain.Booking{}, fmt.Errorf("reverse transfer: 读 biz_date: %w", err)
	}
	if bizDate == "" {
		return domain.Booking{}, fmt.Errorf("reverse transfer: sys_param.biz_date 未设置")
	}

	var booking domain.Booking

	run := pg.RunInTx
	if s.db == nil {
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err = run(ctx, s.db, func(q pg.DBTX) error {
		// Lock all entries of the original voucher (FOR UPDATE) — serializes
		// concurrent reversals of the same voucher.
		origs, err := s.ledgerStore.LockTxnsByVoucher(ctx, q, in.OriginalVoucherNo)
		if err != nil {
			return err
		}
		if len(origs) == 0 {
			return ErrOriginalVoucherNotFound
		}

		// Soft duplicate check: HasReversal reads voucher_reversal. The hard
		// guarantee is the unique PK on reverses_voucher_no (InsertReversal).
		hasRev, err := s.transfers.HasReversalForVoucher(ctx, q, in.OriginalVoucherNo)
		if err != nil {
			return err
		}
		if hasRev {
			return ErrVoucherAlreadyReversed
		}

		ccy := origs[0].Ccy
		reversalVoucher := domain.NewVoucherNo(bizDate)

		// Build reverse entries by flipping each original entry's DC flag.
		// reverseEntries is defined in txn_service.go and shared package-wide.
		entries := reverseEntries(origs)

		// Post the reversal entries as a new voucher; refTxnID links the
		// reversal entries back to the original's first txn for auditing.
		refTxnID := origs[0].TxnID
		txns, err := s.ledger.Post(ctx, q, entries, bizDate, ccy, reversalVoucher, refTxnID)
		if err != nil {
			return err
		}

		if in.Summary != "" {
			if err := s.ledgerStore.SetTxnSummary(ctx, q, reversalVoucher, in.Summary); err != nil {
				return err
			}
			for i := range txns {
				txns[i].Summary = in.Summary
			}
		}

		// Record the reversal mapping. The unique PK on reverses_voucher_no
		// makes a concurrent duplicate InsertReversal fail and roll back.
		if err := s.transfers.InsertReversal(ctx, q, in.OriginalVoucherNo, reversalVoucher); err != nil {
			return err
		}

		// Emit core.transfer-reversed.v1 to the outbox.
		payload, err := buildTransferReversedPayload(reversalVoucher, in.OriginalVoucherNo)
		if err != nil {
			return err
		}
		env := makeTransferResultEnvelope(EventTransferReversed, in.IdempotencyKey, in.SagaRouting, payload)
		if err := s.transfers.AppendOutbox(ctx, q, env, RouteTransferReversed); err != nil {
			return err
		}

		booking = domain.Booking{VoucherNo: reversalVoucher, BizDate: bizDate, Txns: txns}
		return nil
	})
	if err != nil {
		return domain.Booking{}, err
	}
	return booking, nil
}

// --- Outbox payload helpers ---

// makeTransferResultEnvelope stamps the saga routing context from the command
// onto a result envelope, mirroring corebanking.makeResultEnvelope (consumer
// side) and risk.makeResultEnvelope. The service needs its own helper because
// it emits transfer-posted/reversed from within its own transaction, where it
// has the routing fields on the input struct rather than the command envelope.
func makeTransferResultEnvelope(messageType, idempotencyKey string, routing SagaRouting, payload json.RawMessage) messaging.Envelope {
	env := messaging.NewEnvelope(messageType, routing.CorrelationID, payload, time.Now)
	env.WorkflowID = routing.WorkflowID
	env.ActionName = routing.ActionName
	env.CommandID = routing.CommandID
	env.IdempotencyKey = idempotencyKey
	env.CausationID = routing.CommandMessageID
	return env
}

type transferPostedPayload struct {
	VoucherNo   string `json:"voucher_no"`
	HoldID      string `json:"hold_id,omitempty"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

func buildTransferPostedPayload(voucherNo string, in PostHeldTransfer) (json.RawMessage, error) {
	body, err := json.Marshal(transferPostedPayload{
		VoucherNo:   voucherNo,
		HoldID:      in.HoldID,
		FromAccount: in.FromAccount,
		ToAccount:   in.ToAccount,
		AmountCents: in.Amount.Cents(),
		Currency:    in.Ccy,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal transfer-posted payload: %w", err)
	}
	return body, nil
}

type transferReversedPayload struct {
	ReversalVoucherNo string `json:"reversal_voucher_no"`
	OriginalVoucherNo string `json:"original_voucher_no"`
}

func buildTransferReversedPayload(reversalVoucher, originalVoucher string) (json.RawMessage, error) {
	body, err := json.Marshal(transferReversedPayload{
		ReversalVoucherNo: reversalVoucher,
		OriginalVoucherNo: originalVoucher,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal transfer-reversed payload: %w", err)
	}
	return body, nil
}
