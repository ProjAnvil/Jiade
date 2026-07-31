// Package domain is a pure domain model of the risk service.
//
// This file adds the PaymentAuthorization aggregate and the deterministic risk
// policy used by the bank payment saga. The authorization moves through a
// pending → authorized/rejected lifecycle; an authorized authorization may be
// voided. The policy is deterministic so saga tests are stable.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AuthorizationStatus is the lifecycle state of a payment authorization.
type AuthorizationStatus string

const (
	AuthStatusPending    AuthorizationStatus = "pending"
	AuthStatusAuthorized AuthorizationStatus = "authorized"
	AuthStatusRejected   AuthorizationStatus = "rejected"
	AuthStatusVoided     AuthorizationStatus = "voided"
)

// ErrInvalidTransition signals an illegal authorization status transition.
var ErrInvalidTransition = errors.New("invalid authorization transition")

// IsInvalidTransition reports whether err is an invalid-transition error.
func IsInvalidTransition(err error) bool {
	return errors.Is(err, ErrInvalidTransition)
}

// Decision is the outcome of evaluating the risk policy for a payment. When
// Approved is true the authorization may move to authorized; when false it
// moves to rejected. MatchedRuleIDs captures the deterministic rule IDs that
// fired, and ContextDigest is a stable hash of the evaluated context so the
// decision can be audited against the inputs that produced it.
type Decision struct {
	Approved       bool
	MatchedRuleIDs []string
	ContextDigest  string
}

// PaymentAuthorization is the risk-authorization aggregate for a single payment
// instruction. Amounts are represented in int64 minor units (cents) to match
// the rest of the codebase and avoid fractional arithmetic.
type PaymentAuthorization struct {
	AuthorizationID string
	WorkflowID      string
	IdempotencyKey  string
	CustomerID      string
	AmountCents     int64
	Currency        string
	Status          AuthorizationStatus
	MatchedRuleIDs  []string
	ContextDigest   string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewPaymentAuthorization creates a pending authorization with the provided
// identity fields. The caller is expected to call Authorize immediately to
// reach a terminal authorized/rejected state.
func NewPaymentAuthorization(id, workflowID, customerID string, amountCents int64, currency, idempotencyKey string, now time.Time) PaymentAuthorization {
	return PaymentAuthorization{
		AuthorizationID: id,
		WorkflowID:      workflowID,
		IdempotencyKey:  idempotencyKey,
		CustomerID:      customerID,
		AmountCents:     amountCents,
		Currency:        currency,
		Status:          AuthStatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
}

// Decision returns the label for the current state: "approved" when authorized,
// "rejected" when rejected, and "" while pending or voided. This maps directly
// to the decision column persisted by the repository.
func (a PaymentAuthorization) DecisionLabel() string {
	switch a.Status {
	case AuthStatusAuthorized:
		return "approved"
	case AuthStatusRejected:
		return "rejected"
	default:
		return ""
	}
}

// MatchedRuleIDsEqual reports whether the authorization's matched rules equal
// want irrespective of ordering. Both sides are sorted before comparison so the
// method is safe to call on values populated by direct field assignment rather
// than via Authorize.
func (a PaymentAuthorization) MatchedRuleIDsEqual(want []string) bool {
	got := append([]string(nil), a.MatchedRuleIDs...)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(got) != len(sortedWant) {
		return false
	}
	for i, r := range got {
		if r != sortedWant[i] {
			return false
		}
	}
	return true
}

// Authorize applies the policy decision: pending → authorized (approved) or
// pending → rejected (not approved). It records the matched rule IDs (sorted)
// and the context digest. Any non-pending status is an invalid transition,
// preventing voided → authorized and double-decision.
func (a *PaymentAuthorization) Authorize(d Decision) error {
	if a.Status != AuthStatusPending {
		return fmt.Errorf("%w: %s → authorized", ErrInvalidTransition, a.Status)
	}
	if d.Approved {
		a.Status = AuthStatusAuthorized
	} else {
		a.Status = AuthStatusRejected
	}
	rules := append([]string(nil), d.MatchedRuleIDs...)
	sort.Strings(rules)
	a.MatchedRuleIDs = rules
	a.ContextDigest = d.ContextDigest
	return nil
}

// Void transitions authorized → voided. Calling Void on an already-voided
// authorization is idempotent: it returns nil and leaves the state unchanged.
// Pending, rejected, and other transitions are invalid.
func (a *PaymentAuthorization) Void() error {
	switch a.Status {
	case AuthStatusAuthorized:
		a.Status = AuthStatusVoided
		return nil
	case AuthStatusVoided:
		return nil // idempotent
	default:
		return fmt.Errorf("%w: %s → voided", ErrInvalidTransition, a.Status)
	}
}

// ---------------------------------------------------------------------------
// Deterministic risk policy.
//
// The template policy rejects inactive KYC, blacklisted customers, non-positive
// amounts, and seeded high-risk tags. It is deterministic so saga tests are
// stable across runs. Matched rule IDs are returned sorted.
// ---------------------------------------------------------------------------

// Policy rule identifiers. These are recorded on the authorization so the
// reason for rejection is auditable.
const (
	RuleKYCInactive       = "KYC-INACTIVE"
	RuleBlacklisted       = "BLACKLIST"
	RuleAmountNonPositive = "AMOUNT-NON-POSITIVE"
	RuleHighRiskTag       = "HIGH-RISK-TAG"
)

// HighRiskTag is the customer risk tag that triggers deterministic rejection.
// The seed fixtures assign this tag to high-risk-level customers.
const HighRiskTag = "high-risk"

// PolicyContext carries the inputs to the deterministic risk policy.
type PolicyContext struct {
	CustomerID     string
	AmountCents    int64
	KYCStatus      string
	CustomerStatus string
	Blacklisted    bool
	RiskTags       []string
}

// EvaluatePolicy applies the deterministic template policy and returns a
// Decision. When no rules fire the decision is approved; otherwise it is
// rejected with the sorted matched rule IDs. The context digest is a stable
// SHA-256 hash of the canonical policy input.
func EvaluatePolicy(c PolicyContext) Decision {
	matched := evaluateRules(c)
	sort.Strings(matched)
	return Decision{
		Approved:       len(matched) == 0,
		MatchedRuleIDs: matched,
		ContextDigest:  contextDigest(c),
	}
}

func evaluateRules(c PolicyContext) []string {
	var matched []string
	if !kycActive(c.KYCStatus) {
		matched = append(matched, RuleKYCInactive)
	}
	if c.Blacklisted {
		matched = append(matched, RuleBlacklisted)
	}
	if c.AmountCents <= 0 {
		matched = append(matched, RuleAmountNonPositive)
	}
	for _, tag := range c.RiskTags {
		if tag == HighRiskTag {
			matched = append(matched, RuleHighRiskTag)
			break
		}
	}
	return matched
}

// kycActive reports whether the KYC status permits authorization. The bank's
// seed data uses "passed" (Chinese-locale legacy) and the customer gRPC returns
// "verified"; both are accepted.
func kycActive(status string) bool {
	return status == "passed" || status == "verified"
}

// contextDigest returns a stable hex SHA-256 digest of the policy input. The
// canonical form uses a pipe-delimited layout with sorted risk tags so the
// digest is independent of tag ordering.
func contextDigest(c PolicyContext) string {
	tags := append([]string(nil), c.RiskTags...)
	sort.Strings(tags)
	canonical := fmt.Sprintf("%s|%d|%s|%s|%t|%s",
		c.CustomerID, c.AmountCents, c.KYCStatus, c.CustomerStatus, c.Blacklisted,
		strings.Join(tags, ","))
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// EncodeMatchedRules marshals the matched rule IDs to JSON for persistence. Nil
// yields "[]" (not "null") so the NOT NULL DEFAULT '[]' column constraint is
// never violated.
func EncodeMatchedRules(rules []string) ([]byte, error) {
	if rules == nil {
		rules = []string{}
	}
	return json.Marshal(rules)
}

// DecodeMatchedRules unmarshals the matched rule IDs from the persisted JSON.
// An empty or null payload yields a nil slice.
func DecodeMatchedRules(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rules []string
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("decode matched rules: %w", err)
	}
	return rules, nil
}
