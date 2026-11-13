package domain

import (
	"reflect"
	"testing"
	"time"
)

func fixedNow() time.Time {
	return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
}

func approvedDecision() Decision {
	return Decision{Approved: true, MatchedRuleIDs: nil, ContextDigest: "digest-abc"}
}

func rejectedDecision(rules ...string) Decision {
	return Decision{Approved: false, MatchedRuleIDs: rules, ContextDigest: "digest-xyz"}
}

func newPendingAuth() PaymentAuthorization {
	return NewPaymentAuthorization(
		"auth-1", "wf-1", "cust-1", 10000, "CNY", "idem-1", fixedNow(),
	)
}

// TestAuthorize_PendingToApproved transitions pending → authorized with an
// approved decision and preserves matched rule IDs + context digest.
func TestAuthorize_PendingToApproved(t *testing.T) {
	a := newPendingAuth()
	decision := Decision{
		Approved:       true,
		MatchedRuleIDs: []string{"R001", "R002"},
		ContextDigest:  "digest-abc",
	}
	if err := a.Authorize(decision); err != nil {
		t.Fatalf("Authorize approved: %v", err)
	}
	if a.Status != AuthStatusAuthorized {
		t.Errorf("status = %s, want authorized", a.Status)
	}
	if !a.MatchedRuleIDsEqual([]string{"R001", "R002"}) {
		t.Errorf("matched rules = %v, want [R001 R002]", a.MatchedRuleIDs)
	}
	if a.ContextDigest != "digest-abc" {
		t.Errorf("context digest = %q, want digest-abc", a.ContextDigest)
	}
}

// TestAuthorize_PendingToRejected transitions pending → rejected when the
// policy decision is not approved, recording the matched rule IDs.
func TestAuthorize_PendingToRejected(t *testing.T) {
	a := newPendingAuth()
	decision := rejectedDecision("KYC-INACTIVE", "BLACKLIST")
	if err := a.Authorize(decision); err != nil {
		t.Fatalf("Authorize rejected: %v", err)
	}
	if a.Status != AuthStatusRejected {
		t.Errorf("status = %s, want rejected", a.Status)
	}
	if !a.MatchedRuleIDsEqual([]string{"BLACKLIST", "KYC-INACTIVE"}) {
		t.Errorf("matched rules = %v, want sorted [BLACKLIST KYC-INACTIVE]", a.MatchedRuleIDs)
	}
	if a.ContextDigest != "digest-xyz" {
		t.Errorf("context digest = %q, want digest-xyz", a.ContextDigest)
	}
}

// TestVoid_AuthorizedToVoided transitions authorized → voided.
func TestVoid_AuthorizedToVoided(t *testing.T) {
	a := newPendingAuth()
	if err := a.Authorize(approvedDecision()); err != nil {
		t.Fatal(err)
	}
	if err := a.Void(); err != nil {
		t.Fatalf("Void authorized: %v", err)
	}
	if a.Status != AuthStatusVoided {
		t.Errorf("status = %s, want voided", a.Status)
	}
}

// TestVoid_AlreadyVoided_IsIdempotent asserts that Void() on an already-voided
// authorization stays voided without error.
func TestVoid_AlreadyVoided_IsIdempotent(t *testing.T) {
	a := newPendingAuth()
	if err := a.Authorize(approvedDecision()); err != nil {
		t.Fatal(err)
	}
	if err := a.Void(); err != nil {
		t.Fatal(err)
	}
	if err := a.Void(); err != nil {
		t.Fatalf("second Void should be idempotent, got: %v", err)
	}
	if a.Status != AuthStatusVoided {
		t.Errorf("status = %s, want voided", a.Status)
	}
}

// TestAuthorize_VoidedCannotBeAuthorized rejects the voided → authorized
// transition.
func TestAuthorize_VoidedCannotBeAuthorized(t *testing.T) {
	a := newPendingAuth()
	if err := a.Authorize(approvedDecision()); err != nil {
		t.Fatal(err)
	}
	if err := a.Void(); err != nil {
		t.Fatal(err)
	}
	err := a.Authorize(approvedDecision())
	if err == nil {
		t.Fatal("Authorize on voided should fail")
	}
	if !IsInvalidTransition(err) {
		t.Errorf("expected ErrInvalidTransition, got: %v", err)
	}
	if a.Status != AuthStatusVoided {
		t.Errorf("status changed = %s, want voided", a.Status)
	}
}

// TestAuthorize_AlreadyDecided_RejectsTransition asserts that an authorization
// that is already authorized or rejected cannot be re-authorized.
func TestAuthorize_AlreadyDecided_RejectsTransition(t *testing.T) {
	t.Run("authorized", func(t *testing.T) {
		a := newPendingAuth()
		_ = a.Authorize(approvedDecision())
		if err := a.Authorize(rejectedDecision("X")); err == nil {
			t.Fatal("re-authorize on authorized should fail")
		}
	})
	t.Run("rejected", func(t *testing.T) {
		a := newPendingAuth()
		_ = a.Authorize(rejectedDecision("X"))
		if err := a.Authorize(approvedDecision()); err == nil {
			t.Fatal("re-authorize on rejected should fail")
		}
	})
}

// TestVoid_NotAuthorized_RejectsTransition asserts that Void on pending or
// rejected authorizations is an invalid transition.
func TestVoid_NotAuthorized_RejectsTransition(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		a := newPendingAuth()
		if err := a.Void(); err == nil {
			t.Fatal("Void on pending should fail")
		}
	})
	t.Run("rejected", func(t *testing.T) {
		a := newPendingAuth()
		_ = a.Authorize(rejectedDecision("X"))
		if err := a.Void(); err == nil {
			t.Fatal("Void on rejected should fail")
		}
	})
}

// TestNewPaymentAuthorization_Defaults verifies the factory produces a pending
// authorization with the provided identity fields.
func TestNewPaymentAuthorization_Defaults(t *testing.T) {
	a := NewPaymentAuthorization("a1", "w1", "c1", 5000, "USD", "k1", fixedNow())
	if a.AuthorizationID != "a1" || a.WorkflowID != "w1" || a.CustomerID != "c1" {
		t.Errorf("identity fields mismatch: %+v", a)
	}
	if a.AmountCents != 5000 || a.Currency != "USD" {
		t.Errorf("amount/ccy mismatch: %+v", a)
	}
	if a.IdempotencyKey != "k1" {
		t.Errorf("idempotency key = %q, want k1", a.IdempotencyKey)
	}
	if a.Status != AuthStatusPending {
		t.Errorf("status = %s, want pending", a.Status)
	}
	if !a.CreatedAt.Equal(fixedNow()) {
		t.Errorf("created_at = %v, want %v", a.CreatedAt, fixedNow())
	}
}

// ---------------------------------------------------------------------------
// Deterministic policy tests.
// ---------------------------------------------------------------------------

func TestEvaluatePolicy_Approved_NoRules(t *testing.T) {
	d := EvaluatePolicy(PolicyContext{
		CustomerID:     "C1",
		AmountCents:    10000,
		KYCStatus:      "passed",
		CustomerStatus: "active",
		RiskTags:       nil,
	})
	if !d.Approved {
		t.Error("expected approved")
	}
	if len(d.MatchedRuleIDs) != 0 {
		t.Errorf("expected no matched rules, got %v", d.MatchedRuleIDs)
	}
	if d.ContextDigest == "" {
		t.Error("expected non-empty context digest")
	}
}

func TestEvaluatePolicy_RejectsInactiveKYC(t *testing.T) {
	d := EvaluatePolicy(PolicyContext{
		CustomerID: "C1", AmountCents: 10000,
		KYCStatus: "pending",
	})
	if d.Approved {
		t.Error("inactive KYC should reject")
	}
	if !contains(d.MatchedRuleIDs, RuleKYCInactive) {
		t.Errorf("matched = %v, want %s", d.MatchedRuleIDs, RuleKYCInactive)
	}
}

func TestEvaluatePolicy_RejectsBlacklisted(t *testing.T) {
	d := EvaluatePolicy(PolicyContext{
		CustomerID: "C1", AmountCents: 10000,
		KYCStatus:   "passed",
		Blacklisted: true,
	})
	if d.Approved {
		t.Error("blacklisted should reject")
	}
	if !contains(d.MatchedRuleIDs, RuleBlacklisted) {
		t.Errorf("matched = %v, want %s", d.MatchedRuleIDs, RuleBlacklisted)
	}
}

func TestEvaluatePolicy_RejectsNonPositiveAmount(t *testing.T) {
	for _, amt := range []int64{0, -1, -10000} {
		d := EvaluatePolicy(PolicyContext{
			CustomerID: "C1", AmountCents: amt,
			KYCStatus: "passed",
		})
		if d.Approved {
			t.Errorf("amount %d should reject", amt)
		}
		if !contains(d.MatchedRuleIDs, RuleAmountNonPositive) {
			t.Errorf("amount %d: matched = %v, want %s", amt, d.MatchedRuleIDs, RuleAmountNonPositive)
		}
	}
}

func TestEvaluatePolicy_RejectsHighRiskTag(t *testing.T) {
	d := EvaluatePolicy(PolicyContext{
		CustomerID: "C1", AmountCents: 10000,
		KYCStatus: "passed",
		RiskTags:  []string{"low-risk", "high-risk"},
	})
	if d.Approved {
		t.Error("high-risk tag should reject")
	}
	if !contains(d.MatchedRuleIDs, RuleHighRiskTag) {
		t.Errorf("matched = %v, want %s", d.MatchedRuleIDs, RuleHighRiskTag)
	}
}

func TestEvaluatePolicy_DigestIsDeterministic(t *testing.T) {
	ctx := PolicyContext{
		CustomerID: "C1", AmountCents: 10000,
		KYCStatus: "passed", CustomerStatus: "active",
		RiskTags: []string{"x", "y"},
	}
	d1 := EvaluatePolicy(ctx)
	d2 := EvaluatePolicy(ctx)
	if d1.ContextDigest != d2.ContextDigest {
		t.Errorf("digest not deterministic: %s != %s", d1.ContextDigest, d2.ContextDigest)
	}
	// Different input should yield a different digest.
	ctx.AmountCents = 20000
	d3 := EvaluatePolicy(ctx)
	if d3.ContextDigest == d1.ContextDigest {
		t.Error("digest should change when input changes")
	}
}

func TestEvaluatePolicy_MultipleRulesSorted(t *testing.T) {
	d := EvaluatePolicy(PolicyContext{
		CustomerID: "C1", AmountCents: 0,
		KYCStatus:   "pending",
		Blacklisted: true,
		RiskTags:    []string{"high-risk"},
	})
	if d.Approved {
		t.Error("should reject")
	}
	want := []string{RuleAmountNonPositive, RuleBlacklisted, RuleHighRiskTag, RuleKYCInactive}
	if !reflect.DeepEqual(d.MatchedRuleIDs, want) {
		t.Errorf("matched = %v, want %v", d.MatchedRuleIDs, want)
	}
}

func TestKYCActive(t *testing.T) {
	for _, s := range []string{"passed", "verified"} {
		if !kycActive(s) {
			t.Errorf("kycActive(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "pending", "failed", "restricted"} {
		if kycActive(s) {
			t.Errorf("kycActive(%q) = true, want false", s)
		}
	}
}

// --- helpers ---

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
