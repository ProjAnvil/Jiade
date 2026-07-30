package service

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
	"bank/internal/risk/domain"
)

// --- test doubles ---

type fakeStore struct {
	byID         map[string]domain.PaymentAuthorization
	byKey        map[string]domain.PaymentAuthorization
	blacklist    map[string]bool
	inserted     []domain.PaymentAuthorization
	updated      []domain.PaymentAuthorization
	insertErr    error
	updateErr    error
	blacklistErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		byID:      make(map[string]domain.PaymentAuthorization),
		byKey:     make(map[string]domain.PaymentAuthorization),
		blacklist: make(map[string]bool),
	}
}

func (f *fakeStore) Insert(_ context.Context, _ pg.DBTX, auth domain.PaymentAuthorization) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.byID[auth.AuthorizationID] = auth
	f.byKey[auth.IdempotencyKey] = auth
	f.inserted = append(f.inserted, auth)
	return nil
}

func (f *fakeStore) GetByID(_ context.Context, _ pg.DBTX, id string) (domain.PaymentAuthorization, error) {
	a, ok := f.byID[id]
	if !ok {
		return domain.PaymentAuthorization{}, ErrAuthorizationNotFound
	}
	return a, nil
}

func (f *fakeStore) GetByIdempotencyKey(_ context.Context, _ pg.DBTX, key string) (domain.PaymentAuthorization, error) {
	a, ok := f.byKey[key]
	if !ok {
		return domain.PaymentAuthorization{}, ErrAuthorizationNotFound
	}
	return a, nil
}

func (f *fakeStore) UpdateStatus(_ context.Context, _ pg.DBTX, auth domain.PaymentAuthorization) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.byID[auth.AuthorizationID] = auth
	f.byKey[auth.IdempotencyKey] = auth
	f.updated = append(f.updated, auth)
	return nil
}

func (f *fakeStore) IsBlacklisted(_ context.Context, _ pg.DBTX, customerID string) (bool, error) {
	if f.blacklistErr != nil {
		return false, f.blacklistErr
	}
	return f.blacklist[customerID], nil
}

type fakeCustomerReader struct {
	customer serviceclient.Customer
	err      error
}

func (f fakeCustomerReader) GetCustomer(_ context.Context, _, _ string) (serviceclient.Customer, error) {
	return f.customer, f.err
}

func fixedNow() time.Time { return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC) }

func newTestService(store *fakeStore, customer serviceclient.Customer) *AuthorizationService {
	return NewAuthorizationService(store, fakeCustomerReader{customer: customer}, fixedNow)
}

func approvedCustomer() serviceclient.Customer {
	return serviceclient.Customer{
		CustomerID: "C1", Name: "Alice", CustomerType: "personal",
		KYCStatus: "passed", Status: "active",
	}
}

// --- AuthorizePayment tests ---

func TestAuthorizePayment_Approved(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	result, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	if err != nil {
		t.Fatalf("AuthorizePayment: %v", err)
	}
	if result.Duplicate {
		t.Error("should not be duplicate")
	}
	if result.Authorization.Status != domain.AuthStatusAuthorized {
		t.Errorf("status = %s, want authorized", result.Authorization.Status)
	}
	if len(store.inserted) != 1 {
		t.Fatalf("inserted %d, want 1", len(store.inserted))
	}
	persisted := store.inserted[0]
	if persisted.AuthorizationID != "auth-1" || persisted.WorkflowID != "wf-1" {
		t.Errorf("persisted identity mismatch: %+v", persisted)
	}
	if persisted.AmountCents != 10000 || persisted.Currency != "CNY" {
		t.Errorf("persisted amount mismatch: %+v", persisted)
	}
}

func TestAuthorizePayment_Rejected_KYC(t *testing.T) {
	store := newFakeStore()
	cust := approvedCustomer()
	cust.KYCStatus = "pending"
	svc := newTestService(store, cust)

	result, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	if err != nil {
		t.Fatalf("AuthorizePayment: %v", err)
	}
	if result.Authorization.Status != domain.AuthStatusRejected {
		t.Errorf("status = %s, want rejected", result.Authorization.Status)
	}
	if !result.Authorization.MatchedRuleIDsEqual([]string{domain.RuleKYCInactive}) {
		t.Errorf("matched rules = %v, want [KYC-INACTIVE]", result.Authorization.MatchedRuleIDs)
	}
}

func TestAuthorizePayment_Rejected_Blacklist(t *testing.T) {
	store := newFakeStore()
	store.blacklist["C1"] = true
	svc := newTestService(store, approvedCustomer())

	result, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	if err != nil {
		t.Fatalf("AuthorizePayment: %v", err)
	}
	if result.Authorization.Status != domain.AuthStatusRejected {
		t.Errorf("status = %s, want rejected", result.Authorization.Status)
	}
	if !result.Authorization.MatchedRuleIDsEqual([]string{domain.RuleBlacklisted}) {
		t.Errorf("matched rules = %v, want [BLACKLIST]", result.Authorization.MatchedRuleIDs)
	}
}

func TestAuthorizePayment_Rejected_NonPositiveAmount(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	result, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 0, Currency: "CNY",
	})
	if err != nil {
		t.Fatalf("AuthorizePayment: %v", err)
	}
	if result.Authorization.Status != domain.AuthStatusRejected {
		t.Errorf("status = %s, want rejected", result.Authorization.Status)
	}
	if !result.Authorization.MatchedRuleIDsEqual([]string{domain.RuleAmountNonPositive}) {
		t.Errorf("matched rules = %v, want [AMOUNT-NON-POSITIVE]", result.Authorization.MatchedRuleIDs)
	}
}

func TestAuthorizePayment_Rejected_HighRiskTag(t *testing.T) {
	store := newFakeStore()
	cust := approvedCustomer()
	cust.RiskTags = []string{"high-risk"}
	svc := newTestService(store, cust)

	result, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	if err != nil {
		t.Fatalf("AuthorizePayment: %v", err)
	}
	if result.Authorization.Status != domain.AuthStatusRejected {
		t.Errorf("status = %s, want rejected", result.Authorization.Status)
	}
	if !result.Authorization.MatchedRuleIDsEqual([]string{domain.RuleHighRiskTag}) {
		t.Errorf("matched rules = %v, want [HIGH-RISK-TAG]", result.Authorization.MatchedRuleIDs)
	}
}

// TestAuthorizePayment_DuplicateIdempotencyKey verifies that a second call with
// the same idempotency key returns the existing authorization without
// re-evaluating the policy or inserting a duplicate.
func TestAuthorizePayment_DuplicateIdempotencyKey(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	_, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second call with same idempotency key — different authorization ID.
	result, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-2", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Duplicate {
		t.Error("expected duplicate")
	}
	if result.Authorization.AuthorizationID != "auth-1" {
		t.Errorf("duplicate returned wrong auth: %s", result.Authorization.AuthorizationID)
	}
	if len(store.inserted) != 1 {
		t.Errorf("should not insert duplicate: %d inserts", len(store.inserted))
	}
}

func TestAuthorizePayment_CustomerLookupError(t *testing.T) {
	store := newFakeStore()
	svc := NewAuthorizationService(
		store,
		fakeCustomerReader{err: errors.New("gRPC unavailable")},
		fixedNow,
	)
	_, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "idem-1",
		CustomerID: "C1", AmountCents: 10000, Currency: "CNY",
	})
	if err == nil {
		t.Fatal("expected customer lookup error")
	}
}

func TestAuthorizePayment_BlacklistCheckError(t *testing.T) {
	store := newFakeStore()
	store.blacklistErr = errors.New("db unavailable")
	svc := newTestService(store, approvedCustomer())

	_, err := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "idem-1",
		CustomerID: "C1", AmountCents: 10000, Currency: "CNY",
	})
	if err == nil {
		t.Fatal("expected blacklist check error")
	}
}

// --- VoidAuthorization tests ---

func TestVoidAuthorization_AuthorizedToVoided(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	// Seed an authorized authorization.
	_, _ = svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})

	result, err := svc.VoidAuthorization(context.Background(), nil, VoidCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "void-1",
	})
	if err != nil {
		t.Fatalf("VoidAuthorization: %v", err)
	}
	if result.Duplicate {
		t.Error("should not be duplicate")
	}
	if result.Authorization.Status != domain.AuthStatusVoided {
		t.Errorf("status = %s, want voided", result.Authorization.Status)
	}
	if len(store.updated) != 1 {
		t.Errorf("updated %d, want 1", len(store.updated))
	}
}

func TestVoidAuthorization_AlreadyVoided_NoEvent(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	_, _ = svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		IdempotencyKey: "idem-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
	})
	_, _ = svc.VoidAuthorization(context.Background(), nil, VoidCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "void-1",
	})

	// Second void — should be idempotent, no new update.
	result, err := svc.VoidAuthorization(context.Background(), nil, VoidCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "void-2",
	})
	if err != nil {
		t.Fatalf("second void: %v", err)
	}
	if !result.Duplicate {
		t.Error("expected duplicate (already voided)")
	}
	if len(store.updated) != 1 {
		t.Errorf("should not update twice: %d updates", len(store.updated))
	}
}

func TestVoidAuthorization_NotFound(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	_, err := svc.VoidAuthorization(context.Background(), nil, VoidCommand{
		AuthorizationID: "nope", IdempotencyKey: "void-1",
	})
	if !errors.Is(err, ErrAuthorizationNotFound) {
		t.Errorf("expected ErrAuthorizationNotFound, got: %v", err)
	}
}

func TestVoidAuthorization_NotAuthorized(t *testing.T) {
	store := newFakeStore()
	// Seed a rejected authorization (Void on rejected is invalid).
	store.byID["auth-1"] = domain.PaymentAuthorization{
		AuthorizationID: "auth-1", IdempotencyKey: "idem-1",
		Status: domain.AuthStatusRejected,
	}
	svc := newTestService(store, approvedCustomer())

	_, err := svc.VoidAuthorization(context.Background(), nil, VoidCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "void-1",
	})
	if err == nil {
		t.Fatal("expected invalid transition error")
	}
	if !domain.IsInvalidTransition(err) {
		t.Errorf("expected ErrInvalidTransition, got: %v", err)
	}
}

// TestAuthorizeResult_EventType verifies the service classifies the result so
// the consumer knows which result event to emit.
func TestAuthorizeResult_EventType(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	// Approved → "authorized" event.
	r, _ := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "a1", IdempotencyKey: "k1",
		CustomerID: "C1", AmountCents: 1000, Currency: "CNY",
	})
	if r.EventType != AuthorizeEventAuthorized {
		t.Errorf("approved event = %q, want %q", r.EventType, AuthorizeEventAuthorized)
	}

	// Rejected (KYC) → "rejected" event.
	store2 := newFakeStore()
	cust := approvedCustomer()
	cust.KYCStatus = "pending"
	svc2 := newTestService(store2, cust)
	r2, _ := svc2.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "a2", IdempotencyKey: "k2",
		CustomerID: "C1", AmountCents: 1000, Currency: "CNY",
	})
	if r2.EventType != AuthorizeEventRejected {
		t.Errorf("rejected event = %q, want %q", r2.EventType, AuthorizeEventRejected)
	}
}

func TestVoidResult_EventType(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())
	_, _ = svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "a1", WorkflowID: "wf-1", IdempotencyKey: "k1",
		CustomerID: "C1", AmountCents: 1000, Currency: "CNY",
	})
	r, _ := svc.VoidAuthorization(context.Background(), nil, VoidCommand{
		AuthorizationID: "a1", IdempotencyKey: "void-1",
	})
	if r.EventType != VoidEventVoided {
		t.Errorf("void event = %q, want %q", r.EventType, VoidEventVoided)
	}
}

// TestAuthorizePayment_PreservesContextDigest verifies the policy digest is
// stamped on the persisted authorization.
func TestAuthorizePayment_PreservesContextDigest(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store, approvedCustomer())

	r, _ := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "idem-1",
		CustomerID: "C1", AmountCents: 10000, Currency: "CNY",
	})
	if r.Authorization.ContextDigest == "" {
		t.Error("expected non-empty context digest")
	}
	expected := domain.EvaluatePolicy(domain.PolicyContext{
		CustomerID: "C1", AmountCents: 10000,
		KYCStatus: "passed", CustomerStatus: "active",
	}).ContextDigest
	if r.Authorization.ContextDigest != expected {
		t.Errorf("digest mismatch: got %s, want %s", r.Authorization.ContextDigest, expected)
	}
}

// TestAuthorizePayment_MultipleRejectionRules verifies that all matching rules
// are recorded in sorted order.
func TestAuthorizePayment_MultipleRejectionRules(t *testing.T) {
	store := newFakeStore()
	store.blacklist["C1"] = true
	cust := approvedCustomer()
	cust.KYCStatus = "pending"
	cust.RiskTags = []string{"high-risk"}
	svc := newTestService(store, cust)

	r, _ := svc.AuthorizePayment(context.Background(), nil, AuthorizeCommand{
		AuthorizationID: "auth-1", IdempotencyKey: "idem-1",
		CustomerID: "C1", AmountCents: 0, Currency: "CNY",
	})
	if r.Authorization.Status != domain.AuthStatusRejected {
		t.Errorf("status = %s, want rejected", r.Authorization.Status)
	}
	want := []string{
		domain.RuleAmountNonPositive,
		domain.RuleBlacklisted,
		domain.RuleHighRiskTag,
		domain.RuleKYCInactive,
	}
	if !reflect.DeepEqual(r.Authorization.MatchedRuleIDs, want) {
		t.Errorf("matched = %v, want %v", r.Authorization.MatchedRuleIDs, want)
	}
}
