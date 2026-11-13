// Package testfail houses deterministic, fixture-derived failure-injection
// controls used ONLY by the bank smoke suite (test/smoke.sh).
//
// The controls let a smoke test drive a saga into a specific failure +
// recovery path (risk reject, insufficient funds, transient transfer
// failure, compensation exhaustion) by submitting a payment whose
// Idempotency-Key carries one of the smoke prefixes below. The payment
// service propagates the prefix into the workflow_id (see
// WorkflowIDForSmoke), and each downstream consumer matches the
// workflow_id to inject the corresponding outcome.
//
// Safety contract:
//
//  1. Every control is gated by BANK_TEST_FAILURES_ENABLED=true. The
//     production compose.yaml NEVER sets this variable, so the controls
//     are inert in any deployment that does not explicitly opt in.
//  2. The controls are fixture-derived: only an Idempotency-Key that
//     starts with one of the well-known smoke prefixes activates a
//     branch. An arbitrary external request that does not know the
//     prefix cannot trigger a failure outcome.
//  3. The smoke prefixes can NEVER collide with a server-generated
//     workflow_id. The payment service generates workflow ids as
//     "wf-<uuid>" (32 hex chars, no dashes); a smoke workflow_id is
//     "wf-smoke-<scenario>-<suffix>", whose literal "smoke-" segment
//     cannot be produced by hex UUID generation.
package testfail

import (
	"os"
	"strings"
	"sync"
)

// Smoke idempotency-key prefixes. A smoke test sets the Idempotency-Key
// header to one of these prefixes (plus a unique suffix) to drive the
// corresponding failure-injection branch. Each prefix maps to exactly
// one smoke gate in test/smoke.sh.
const (
	// PrefixReject — risk authorization rejects the payment.
	// Gate: risk rejection → no hold/voucher.
	PrefixReject = "smoke-reject-"

	// PrefixInsuff — funds hold returns insufficient available balance.
	// Gate: insufficient funds → authorization voided by compensation.
	PrefixInsuff = "smoke-insuff-"

	// PrefixTransient — first transfer attempt fails terminally
	// (business_rejected), triggering compensation that releases the
	// hold. The saga ends in compensated; the hold ends released.
	// Gate: transient transfer failure → hold release.
	PrefixTransient = "smoke-transient-"

	// PrefixCompFail — release-hold compensation always reports a
	// transient failure so the saga retries the compensation up to
	// CompensationMaxAttempts and then transitions to
	// compensation_failed.
	// Gate: compensation exhaustion → compensation_failed.
	PrefixCompFail = "smoke-compfail-"
)

// allPrefixes is the complete set of smoke idempotency-key prefixes.
// WorkflowIDForSmoke uses it to detect a smoke request.
var allPrefixes = []string{
	PrefixReject,
	PrefixInsuff,
	PrefixTransient,
	PrefixCompFail,
}

var (
	checkOnce sync.Once
	enabled   bool
)

// Enabled reports whether deterministic test-only failure injection is
// active. Gated by BANK_TEST_FAILURES_ENABLED=true so production
// deployments cannot trigger the smoke branches even if a smoke
// idempotency-key is somehow presented to the service.
func Enabled() bool {
	checkOnce.Do(func() {
		enabled = os.Getenv("BANK_TEST_FAILURES_ENABLED") == "true"
	})
	return enabled
}

// WorkflowIDForSmoke derives a workflow_id from idempotencyKey when
// failure injection is enabled and the key carries one of the smoke
// prefixes. The returned workflow_id is "wf-<idempotencyKey>" so the
// smoke prefix propagates into the workflow_id, which downstream
// consumers match (via IsReject &c.) to inject the corresponding
// failure.
//
// When failure injection is disabled, or the key does not carry a smoke
// prefix, the function returns ("", false) and the caller falls back to
// the default server-generated workflow_id. This makes the smoke path
// a strict no-op in production.
func WorkflowIDForSmoke(idempotencyKey string) (string, bool) {
	if !Enabled() {
		return "", false
	}
	if !hasSmokePrefix(idempotencyKey) {
		return "", false
	}
	return "wf-" + idempotencyKey, true
}

// hasSmokePrefix reports whether idempotencyKey starts with any of the
// smoke prefixes. It is only called after Enabled() has returned true,
// so the prefix check never runs in production.
func hasSmokePrefix(idempotencyKey string) bool {
	for _, p := range allPrefixes {
		if strings.HasPrefix(idempotencyKey, p) {
			return true
		}
	}
	return false
}

// IsReject reports whether workflowID corresponds to the smoke-reject
// scenario. Always false when failure injection is disabled.
func IsReject(workflowID string) bool { return scenarioMatch(workflowID, PrefixReject) }

// IsInsuff reports whether workflowID corresponds to the smoke-insuff
// scenario. Always false when failure injection is disabled.
func IsInsuff(workflowID string) bool { return scenarioMatch(workflowID, PrefixInsuff) }

// IsTransient reports whether workflowID corresponds to the
// smoke-transient scenario. Always false when failure injection is
// disabled.
func IsTransient(workflowID string) bool { return scenarioMatch(workflowID, PrefixTransient) }

// IsCompFail reports whether workflowID corresponds to the
// smoke-compfail scenario. Always false when failure injection is
// disabled.
func IsCompFail(workflowID string) bool { return scenarioMatch(workflowID, PrefixCompFail) }

// scenarioMatch reports whether workflowID starts with the wire form of
// the given smoke prefix. The wire form is "wf-"+prefix because
// WorkflowIDForSmoke prepends "wf-" when propagating the idempotency
// key into the workflow_id. Returns false when failure injection is
// disabled, regardless of the workflow_id, so the smoke branches are
// inert in production.
func scenarioMatch(workflowID, prefix string) bool {
	if !Enabled() {
		return false
	}
	return strings.HasPrefix(workflowID, "wf-"+prefix)
}
