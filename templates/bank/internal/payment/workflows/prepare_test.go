package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"bank/internal/platform/serviceclient"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// Fakes — implement the serviceclient reader interfaces directly so tests can
// drive the retry policy with real gRPC status errors.
// ---------------------------------------------------------------------------

type fakeCustomerReader struct {
	get   func(ctx context.Context, customerID, requestID string) (serviceclient.Customer, error)
	calls atomic.Int32
}

func (f *fakeCustomerReader) GetCustomer(ctx context.Context, customerID, requestID string) (serviceclient.Customer, error) {
	f.calls.Add(1)
	return f.get(ctx, customerID, requestID)
}

type fakeAccountReader struct {
	get   func(ctx context.Context, accountNo, requestID string) (serviceclient.Account, error)
	calls atomic.Int32
}

func (f *fakeAccountReader) GetAccount(ctx context.Context, accountNo, requestID string) (serviceclient.Account, error) {
	f.calls.Add(1)
	return f.get(ctx, accountNo, requestID)
}

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

func validInput() PrepareInput {
	return PrepareInput{
		PaymentID:       "PAY-1",
		PayerCustomerID: "C-100",
		PayerAccountNo:  "ACC-PAYER",
		PayeeAccountNo:  "ACC-PAYEE",
		Currency:        "CNY",
		AmountMinor:     50000, // 500.00
	}
}

func validCustomerReader() *fakeCustomerReader {
	return &fakeCustomerReader{
		get: func(context.Context, string, string) (serviceclient.Customer, error) {
			return serviceclient.Customer{
				CustomerID: "C-100",
				Name:       "Alice",
				Status:     "active",
				KYCStatus:  "verified",
			}, nil
		},
	}
}

func validAccountReader() *fakeAccountReader {
	return &fakeAccountReader{
		get: func(_ context.Context, accountNo, _ string) (serviceclient.Account, error) {
			switch accountNo {
			case "ACC-PAYER":
				return serviceclient.Account{
					AccountNo:             "ACC-PAYER",
					CustomerID:            "C-100",
					Currency:              "CNY",
					Status:                "active",
					LedgerBalanceMinor:    100000,
					AvailableBalanceMinor: 80000,
				}, nil
			case "ACC-PAYEE":
				return serviceclient.Account{
					AccountNo:             "ACC-PAYEE",
					CustomerID:            "C-200",
					Currency:              "CNY",
					Status:                "active",
					LedgerBalanceMinor:    50000,
					AvailableBalanceMinor: 50000,
				}, nil
			}
			return serviceclient.Account{}, status.Error(codes.NotFound, "not found")
		},
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func assertErrorIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func unmarshalContext(t *testing.T, out json.RawMessage) TransferContext {
	t.Helper()
	var tc TransferContext
	if err := json.Unmarshal(out, &tc); err != nil {
		t.Fatalf("unmarshal transfer context: %v", err)
	}
	return tc
}

// ---------------------------------------------------------------------------
// Step 1: Preparation tests.
// ---------------------------------------------------------------------------

func TestPrepare_Success_BuildsImmutableContext(t *testing.T) {
	customers := validCustomerReader()
	accounts := validAccountReader()
	p := NewPreparation(customers, accounts)

	out, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tc := unmarshalContext(t, out)

	want := TransferContext{
		PaymentID:              "PAY-1",
		PayerCustomerID:        "C-100",
		PayerAccountNo:         "ACC-PAYER",
		PayeeAccountNo:         "ACC-PAYEE",
		Currency:               "CNY",
		AmountMinor:            50000,
		PayerLedgerSnapshot:    100000,
		PayerAvailableSnapshot: 80000,
		CustomerKYC:            "verified",
	}
	// Compare everything except the digest (tested separately).
	tcCopy := tc
	tcCopy.ContextDigest = ""
	if tcCopy != want {
		t.Errorf("context = %+v, want %+v", tcCopy, want)
	}
	if tc.ContextDigest == "" {
		t.Errorf("context digest is empty")
	}
}

func TestPrepare_ParallelReads_AllCalledOnce(t *testing.T) {
	customers := validCustomerReader()
	accounts := validAccountReader()
	p := NewPreparation(customers, accounts)

	_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := customers.calls.Load(); got != 1 {
		t.Errorf("customer reads = %d, want 1", got)
	}
	if got := accounts.calls.Load(); got != 2 {
		t.Errorf("account reads = %d, want 2 (payer + payee)", got)
	}
}

func TestPrepare_ParallelReads_RunConcurrently(t *testing.T) {
	delay := 80 * time.Millisecond
	customers := &fakeCustomerReader{
		get: func(context.Context, string, string) (serviceclient.Customer, error) {
			time.Sleep(delay)
			return serviceclient.Customer{CustomerID: "C-100", Status: "active", KYCStatus: "verified"}, nil
		},
	}
	accounts := &fakeAccountReader{
		get: func(_ context.Context, accountNo, _ string) (serviceclient.Account, error) {
			time.Sleep(delay)
			return serviceclient.Account{AccountNo: accountNo, Currency: "CNY", Status: "active"}, nil
		},
	}
	p := NewPreparation(customers, accounts)

	start := time.Now()
	_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// Three 80ms reads in parallel → ~80ms total, never 240ms.
	if elapsed >= 2*delay {
		t.Errorf("reads were not parallel: elapsed = %v, want < %v", elapsed, 2*delay)
	}
}

func TestPrepare_RejectsSameAccount_NoRemoteCalls(t *testing.T) {
	customers := validCustomerReader()
	accounts := validAccountReader()
	p := NewPreparation(customers, accounts)

	input := validInput()
	input.PayerAccountNo = "ACC-SAME"
	input.PayeeAccountNo = "ACC-SAME"
	_, err := p.Prepare(context.Background(), mustJSON(t, input))
	assertErrorIs(t, err, ErrSameAccount)
	if got := customers.calls.Load(); got != 0 {
		t.Errorf("customer calls = %d, want 0 (fail fast)", got)
	}
	if got := accounts.calls.Load(); got != 0 {
		t.Errorf("account calls = %d, want 0 (fail fast)", got)
	}
}

func TestPrepare_RejectsNonPositiveAmount_NoRemoteCalls(t *testing.T) {
	for name, amount := range map[string]int64{
		"zero":     0,
		"negative": -100,
	} {
		t.Run(name, func(t *testing.T) {
			customers := validCustomerReader()
			accounts := validAccountReader()
			p := NewPreparation(customers, accounts)

			input := validInput()
			input.AmountMinor = amount
			_, err := p.Prepare(context.Background(), mustJSON(t, input))
			assertErrorIs(t, err, ErrNonPositiveAmount)
			if got := customers.calls.Load(); got != 0 {
				t.Errorf("customer calls = %d, want 0 (fail fast)", got)
			}
			if got := accounts.calls.Load(); got != 0 {
				t.Errorf("account calls = %d, want 0 (fail fast)", got)
			}
		})
	}
}

func TestPrepare_RejectsMismatchedCurrency(t *testing.T) {
	for name, tc := range map[string]struct {
		payerCcy string
		payeeCcy string
	}{
		"payer mismatch": {"USD", "CNY"},
		"payee mismatch": {"CNY", "USD"},
		"both mismatch":  {"USD", "EUR"},
	} {
		t.Run(name, func(t *testing.T) {
			accounts := &fakeAccountReader{
				get: func(_ context.Context, accountNo, _ string) (serviceclient.Account, error) {
					ccy := tc.payerCcy
					if accountNo == "ACC-PAYEE" {
						ccy = tc.payeeCcy
					}
					return serviceclient.Account{AccountNo: accountNo, Currency: ccy, Status: "active"}, nil
				},
			}
			p := NewPreparation(validCustomerReader(), accounts)

			_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
			assertErrorIs(t, err, ErrCurrencyMismatch)
		})
	}
}

func TestPrepare_RejectsInactiveCustomer(t *testing.T) {
	customers := &fakeCustomerReader{
		get: func(context.Context, string, string) (serviceclient.Customer, error) {
			return serviceclient.Customer{CustomerID: "C-100", Status: "suspended", KYCStatus: "verified"}, nil
		},
	}
	p := NewPreparation(customers, validAccountReader())

	_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	assertErrorIs(t, err, ErrCustomerInactive)
}

func TestPrepare_RejectsUnverifiedKYC(t *testing.T) {
	customers := &fakeCustomerReader{
		get: func(context.Context, string, string) (serviceclient.Customer, error) {
			return serviceclient.Customer{CustomerID: "C-100", Status: "active", KYCStatus: "pending"}, nil
		},
	}
	p := NewPreparation(customers, validAccountReader())

	_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	assertErrorIs(t, err, ErrKYCNotVerified)
}

func TestPrepare_RejectsClosedAccount(t *testing.T) {
	for name, accountNo := range map[string]string{
		"payer": "ACC-PAYER",
		"payee": "ACC-PAYEE",
	} {
		t.Run(name, func(t *testing.T) {
			accounts := &fakeAccountReader{
				get: func(_ context.Context, no, _ string) (serviceclient.Account, error) {
					st := "active"
					if no == accountNo {
						st = "closed"
					}
					return serviceclient.Account{AccountNo: no, Currency: "CNY", Status: st}, nil
				},
			}
			p := NewPreparation(validCustomerReader(), accounts)

			_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
			assertErrorIs(t, err, ErrAccountClosed)
		})
	}
}

func TestPrepare_RejectsFrozenAccount(t *testing.T) {
	for name, accountNo := range map[string]string{
		"payer": "ACC-PAYER",
		"payee": "ACC-PAYEE",
	} {
		t.Run(name, func(t *testing.T) {
			accounts := &fakeAccountReader{
				get: func(_ context.Context, no, _ string) (serviceclient.Account, error) {
					st := "active"
					if no == accountNo {
						st = "frozen"
					}
					return serviceclient.Account{AccountNo: no, Currency: "CNY", Status: st}, nil
				},
			}
			p := NewPreparation(validCustomerReader(), accounts)

			_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
			assertErrorIs(t, err, ErrAccountFrozen)
		})
	}
}

func TestPrepare_BalanceSnapshotStoredDoesNotAuthorize(t *testing.T) {
	p := NewPreparation(validCustomerReader(), validAccountReader())

	out, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tc := unmarshalContext(t, out)

	// The snapshot IS stored — matching the payer account's observed balances.
	if tc.PayerLedgerSnapshot != 100000 {
		t.Errorf("PayerLedgerSnapshot = %d, want 100000", tc.PayerLedgerSnapshot)
	}
	if tc.PayerAvailableSnapshot != 80000 {
		t.Errorf("PayerAvailableSnapshot = %d, want 80000", tc.PayerAvailableSnapshot)
	}

	// The context does NOT claim authorization: the struct carries no field
	// asserting that funds are authorized, held, reserved, or approved. That
	// is the responsibility of the risk + hold actions (Tasks 1-2), which run
	// later in the saga and record their own state. The snapshot is purely an
	// observation of the payer's balance at Preparation time.
	var raw map[string]any
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if len(raw) != 10 {
		t.Errorf("context has %d top-level keys, want exactly 10", len(raw))
	}
	for _, key := range []string{"authorized", "approved", "held", "reserved", "funds_held"} {
		if _, ok := raw[key]; ok {
			t.Errorf("context must not carry authorization key %q", key)
		}
	}
}

// ---------------------------------------------------------------------------
// Retry policy tests.
// ---------------------------------------------------------------------------

func TestPrepare_RetriesUnavailableThenSucceeds(t *testing.T) {
	customers := &fakeCustomerReader{}
	customers.get = func(context.Context, string, string) (serviceclient.Customer, error) {
		if customers.calls.Load() == 1 {
			return serviceclient.Customer{}, status.Error(codes.Unavailable, "try again")
		}
		return serviceclient.Customer{CustomerID: "C-100", Status: "active", KYCStatus: "verified"}, nil
	}
	p := NewPreparation(customers, validAccountReader())

	out, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := customers.calls.Load(); got != 2 {
		t.Errorf("customer reads = %d, want 2 (initial + retry)", got)
	}
	tc := unmarshalContext(t, out)
	if tc.PaymentID != "PAY-1" {
		t.Errorf("payment_id = %q", tc.PaymentID)
	}
}

func TestPrepare_RetriesResourceExhaustedThenSucceeds(t *testing.T) {
	// Per-account counter so the retry condition is independent of the
	// interleaving between the parallel payer and payee goroutines.
	var payerCalls atomic.Int32
	accounts := &fakeAccountReader{}
	accounts.get = func(_ context.Context, accountNo, _ string) (serviceclient.Account, error) {
		if accountNo == "ACC-PAYER" && payerCalls.Add(1) == 1 {
			return serviceclient.Account{}, status.Error(codes.ResourceExhausted, "rate limited")
		}
		switch accountNo {
		case "ACC-PAYER":
			return serviceclient.Account{AccountNo: accountNo, Currency: "CNY", Status: "active", LedgerBalanceMinor: 100, AvailableBalanceMinor: 100}, nil
		default:
			return serviceclient.Account{AccountNo: accountNo, Currency: "CNY", Status: "active"}, nil
		}
	}
	p := NewPreparation(validCustomerReader(), accounts)

	_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := payerCalls.Load(); got != 2 {
		t.Errorf("payer account reads = %d, want 2 (initial + retry)", got)
	}
}

func TestPrepare_RetriesUntilExhausted_ReturnsLastError(t *testing.T) {
	customers := &fakeCustomerReader{
		get: func(context.Context, string, string) (serviceclient.Customer, error) {
			return serviceclient.Customer{}, status.Error(codes.Unavailable, "permanently down")
		},
	}
	p := NewPreparation(customers, validAccountReader())

	_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err == nil {
		t.Fatalf("expected error after retries exhausted")
	}
	if got := customers.calls.Load(); got != 2 {
		t.Errorf("customer reads = %d, want 2 (max attempts)", got)
	}
	if status.Code(err) != codes.Unavailable {
		t.Errorf("error code = %s, want Unavailable", status.Code(err))
	}
}

func TestPrepare_DoesNotRetryNonRetryableGRPC(t *testing.T) {
	for name, code := range map[string]codes.Code{
		"NotFound":        codes.NotFound,
		"InvalidArgument": codes.InvalidArgument,
	} {
		t.Run(name, func(t *testing.T) {
			customers := &fakeCustomerReader{
				get: func(context.Context, string, string) (serviceclient.Customer, error) {
					return serviceclient.Customer{}, status.Error(code, name)
				},
			}
			p := NewPreparation(customers, validAccountReader())

			_, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
			if err == nil {
				t.Fatalf("expected error")
			}
			if got := customers.calls.Load(); got != 1 {
				t.Errorf("customer reads = %d, want 1 (no retry for %s)", got, name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Digest tests.
// ---------------------------------------------------------------------------

func TestPrepare_DigestIsCanonicalSHA256(t *testing.T) {
	p := NewPreparation(validCustomerReader(), validAccountReader())

	out, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	tc := unmarshalContext(t, out)

	// Independently recompute the digest: SHA-256 over canonical (sorted-key)
	// JSON of the TransferContext excluding the context_digest field.
	rawMap := map[string]any{
		"payment_id":               tc.PaymentID,
		"payer_customer_id":        tc.PayerCustomerID,
		"payer_account_no":         tc.PayerAccountNo,
		"payee_account_no":         tc.PayeeAccountNo,
		"currency":                 tc.Currency,
		"amount_minor":             tc.AmountMinor,
		"payer_ledger_snapshot":    tc.PayerLedgerSnapshot,
		"payer_available_snapshot": tc.PayerAvailableSnapshot,
		"customer_kyc":             tc.CustomerKYC,
	}
	canonical, err := json.Marshal(rawMap) // Go sorts map keys alphabetically
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	sum := sha256.Sum256(canonical)
	want := hex.EncodeToString(sum[:])

	if tc.ContextDigest != want {
		t.Errorf("digest mismatch:\n got  %s\n want %s", tc.ContextDigest, want)
	}
}

func TestPrepare_DigestIsDeterministic(t *testing.T) {
	p := NewPreparation(validCustomerReader(), validAccountReader())

	out1, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare (1): %v", err)
	}
	out2, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare (2): %v", err)
	}
	tc1 := unmarshalContext(t, out1)
	tc2 := unmarshalContext(t, out2)
	if tc1.ContextDigest != tc2.ContextDigest {
		t.Errorf("digest not deterministic: %q vs %q", tc1.ContextDigest, tc2.ContextDigest)
	}
}

// ---------------------------------------------------------------------------
// Engine-level integration: Prepare output is valid JSON RawMessage.
// ---------------------------------------------------------------------------

func TestPrepare_OutputIsJSONRawMessage(t *testing.T) {
	p := NewPreparation(validCustomerReader(), validAccountReader())

	out, err := p.Prepare(context.Background(), mustJSON(t, validInput()))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("output is not valid JSON: %s", string(out))
	}
}

// ---------------------------------------------------------------------------
// Compiler guard: fakes satisfy the interfaces.
// ---------------------------------------------------------------------------

var (
	_ serviceclient.CustomerReader = (*fakeCustomerReader)(nil)
	_ serviceclient.AccountReader  = (*fakeAccountReader)(nil)
)
