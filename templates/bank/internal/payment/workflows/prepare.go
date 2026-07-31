package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"bank/internal/platform/serviceclient"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Preparation phase deadlines and retry policy (see task-5-brief Step 3).
const (
	// prepareTimeout bounds the entire Preparation phase. The engine calls
	// Prepare outside any transaction; this deadline prevents a stuck
	// downstream service from blocking the workflow indefinitely.
	prepareTimeout = 5 * time.Second
	// perAttemptTimeout bounds a single gRPC read attempt. Expired per-attempt
	// deadlines surface as context.DeadlineExceeded and are NOT retried.
	perAttemptTimeout = 2 * time.Second
	// maxReadAttempts caps total attempts per read at two (one initial plus
	// one retry) for transient read-only failures.
	maxReadAttempts = 2
	// Backoff parameters for retries: exponential base with bounded jitter.
	backoffBase   = 100 * time.Millisecond
	backoffJitter = 50 * time.Millisecond
)

// Validation errors returned by Prepare. Each represents a terminal rejection
// — the engine records StatusRejected and the saga never starts. They are
// sentinel errors so callers can distinguish rejection causes via errors.Is.
var (
	ErrNonPositiveAmount = errors.New("prepare: amount must be positive")
	ErrSameAccount       = errors.New("prepare: payer and payee accounts must differ")
	ErrCurrencyMismatch  = errors.New("prepare: account currency does not match transfer currency")
	ErrCustomerInactive  = errors.New("prepare: payer customer is not active")
	ErrKYCNotVerified    = errors.New("prepare: payer customer KYC is not verified")
	ErrAccountClosed     = errors.New("prepare: account is closed")
	ErrAccountFrozen     = errors.New("prepare: account is frozen")
	ErrAccountNotActive  = errors.New("prepare: account is not active")
)

// PrepareInput is the JSON payload the engine passes to Definition.Prepare. It
// carries only the request-originated fields; everything else in the
// TransferContext comes from gRPC reads or computation.
type PrepareInput struct {
	PaymentID       string `json:"payment_id"`
	PayerCustomerID string `json:"payer_customer_id"`
	PayerAccountNo  string `json:"payer_account_no"`
	PayeeAccountNo  string `json:"payee_account_no"`
	Currency        string `json:"currency"`
	AmountMinor     int64  `json:"amount_minor"`
}

// Preparation reads customer and account snapshots over gRPC in parallel,
// validates them, and produces an immutable TransferContext. It is the
// Preparation phase of the payment-transfer workflow Definition; the engine
// calls Prepare outside any database transaction.
type Preparation struct {
	customers serviceclient.CustomerReader
	accounts  serviceclient.AccountReader
}

// NewPreparation builds a Preparation that reads from the given gRPC adapters.
// Both readers must be non-nil; Preparation panics on a nil dependency to fail
// fast at wiring time rather than at the first request.
func NewPreparation(customers serviceclient.CustomerReader, accounts serviceclient.AccountReader) *Preparation {
	if customers == nil {
		panic("workflows: NewPreparation requires a non-nil CustomerReader")
	}
	if accounts == nil {
		panic("workflows: NewPreparation requires a non-nil AccountReader")
	}
	return &Preparation{customers: customers, accounts: accounts}
}

// Prepare executes the Preparation phase and returns the immutable
// TransferContext as JSON for the engine to store on
// Instance.PreparedContext. The returned JSON is never mutated by subsequent
// actions.
func (p *Preparation) Prepare(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	// Bound the whole Preparation phase.
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()

	var in PrepareInput
	if err := json.Unmarshal(input, &in); err != nil {
		return nil, fmt.Errorf("prepare: invalid input: %w", err)
	}

	// Fail-fast input validations: these need no remote reads and short-circuit
	// before consuming any downstream resources.
	if in.AmountMinor <= 0 {
		return nil, ErrNonPositiveAmount
	}
	if in.PayerAccountNo == in.PayeeAccountNo {
		return nil, ErrSameAccount
	}

	// Parallel customer + account reads. errgroup.WithContext cancels sibling
	// reads on the first error so a single failure does not block on the
	// remaining attempts.
	var (
		customer     serviceclient.Customer
		payerAccount serviceclient.Account
		payeeAccount serviceclient.Account
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		c, err := withRetry(gctx, func(c context.Context) (serviceclient.Customer, error) {
			return p.customers.GetCustomer(c, in.PayerCustomerID, in.PaymentID)
		})
		if err != nil {
			return err
		}
		customer = c
		return nil
	})
	g.Go(func() error {
		a, err := withRetry(gctx, func(c context.Context) (serviceclient.Account, error) {
			return p.accounts.GetAccount(c, in.PayerAccountNo, in.PaymentID)
		})
		if err != nil {
			return err
		}
		payerAccount = a
		return nil
	})
	g.Go(func() error {
		a, err := withRetry(gctx, func(c context.Context) (serviceclient.Account, error) {
			return p.accounts.GetAccount(c, in.PayeeAccountNo, in.PaymentID)
		})
		if err != nil {
			return err
		}
		payeeAccount = a
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Business-state validations against the freshly-read snapshots.
	if err := validateCustomer(customer); err != nil {
		return nil, err
	}
	if err := validateAccount(payerAccount, "payer"); err != nil {
		return nil, err
	}
	if err := validateAccount(payeeAccount, "payee"); err != nil {
		return nil, err
	}
	if payerAccount.Currency != in.Currency || payeeAccount.Currency != in.Currency {
		return nil, ErrCurrencyMismatch
	}

	tc := TransferContext{
		PaymentID:              in.PaymentID,
		PayerCustomerID:        in.PayerCustomerID,
		PayerAccountNo:         in.PayerAccountNo,
		PayeeAccountNo:         in.PayeeAccountNo,
		Currency:               in.Currency,
		AmountMinor:            in.AmountMinor,
		PayerLedgerSnapshot:    payerAccount.LedgerBalanceMinor,
		PayerAvailableSnapshot: payerAccount.AvailableBalanceMinor,
		CustomerKYC:            customer.KYCStatus,
	}
	digest, err := tc.ComputeDigest()
	if err != nil {
		return nil, err
	}
	tc.ContextDigest = digest

	return json.Marshal(tc)
}

// validateCustomer rejects inactive customers and unverified KYC.
func validateCustomer(c serviceclient.Customer) error {
	if c.Status != "active" {
		return ErrCustomerInactive
	}
	if !kycVerified(c.KYCStatus) {
		return ErrKYCNotVerified
	}
	return nil
}

// kycVerified reports whether the KYC status permits a payment. The bank
// seed fixtures use "passed" (legacy locale spelling) and the customer
// gRPC may also return "verified"; both are accepted, mirroring the risk
// domain's kycActive helper so a seeded customer can actually clear the
// Preparation gate.
func kycVerified(status string) bool {
	return status == "verified" || status == "passed"
}

// validateAccount rejects closed, frozen, or otherwise non-active accounts.
// The role argument ("payer" / "payee") is included in the generic-status
// error message for operational debuggability.
func validateAccount(a serviceclient.Account, role string) error {
	switch a.Status {
	case "active":
		return nil
	case "closed":
		return ErrAccountClosed
	case "frozen":
		return ErrAccountFrozen
	default:
		return fmt.Errorf("%w: %s account %s has status %q", ErrAccountNotActive, role, a.AccountNo, a.Status)
	}
}

// withRetry calls fn up to maxReadAttempts times. Read-only transient gRPC
// errors (Unavailable, ResourceExhausted) are retried with exponential backoff
// and jitter. All other errors — InvalidArgument, NotFound, business-state
// rejections, context cancellation — are returned immediately so the engine
// can reject or surface them without delay.
func withRetry[T any](ctx context.Context, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt < maxReadAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(readBackoff(attempt)):
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, perAttemptTimeout)
		result, err := fn(callCtx)
		cancel()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !isRetryableGRPC(err) {
			return zero, err
		}
	}
	return zero, lastErr
}

// readBackoff computes the delay before attempt N (1-indexed: the first retry
// is attempt=1). Exponential base doubles each step; jitter spreads retries
// across a 0..backoffJitter window to avoid thundering herds.
func readBackoff(attempt int) time.Duration {
	exp := backoffBase * (1 << (attempt - 1)) // attempt=1 → 100ms, attempt=2 → 200ms
	jitter := time.Duration(rand.Int64N(int64(backoffJitter)))
	return exp + jitter
}

// isRetryableGRPC reports whether err is a read-only transient gRPC failure
// that is safe to retry. status.Code returns codes.OK for nil and codes.Unknown
// for non-gRPC errors, neither of which are retryable.
func isRetryableGRPC(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}
