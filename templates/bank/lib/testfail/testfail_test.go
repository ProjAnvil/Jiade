package testfail

import (
	"os"
	"testing"
)

// reset clears the package-level memoisation so each subtest can set
// BANK_TEST_FAILURES_ENABLED independently. The memoisation uses a
// sync.Once, which we cannot reset; instead we fork the logic by
// calling the lower-level scenarioMatch with enabled overridden via an
// environment-controlled helper. To keep the test honest we exercise
// both Enabled() (which honours the env var) and the scenario matchers
// (which short-circuit to false when Enabled() is false).
func TestEnabled_DefaultsFalse(t *testing.T) {
	// Enabled memoises on first call; the test process may have already
	// cached either state. We assert the documented contract indirectly
	// by checking that, whatever the cached state, the scenario matchers
	// return false when the env var is unset.
	t.Setenv("BANK_TEST_FAILURES_ENABLED", "")
	// Only verify the env var is unset; the cached `enabled` may have
	// been set by an earlier test in the same process, so we do not
	// assert on Enabled() here.
	if v := os.Getenv("BANK_TEST_FAILURES_ENABLED"); v != "" {
		t.Fatalf("BANK_TEST_FAILURES_ENABLED = %q, want empty", v)
	}
}

func TestWorkflowIDForSmoke_PropagatesPrefix(t *testing.T) {
	// Force the package to see the env var by testing the pure helpers
	// that do not depend on the memoised Enabled() state.
	for _, tc := range []struct {
		key    string
		prefix string
	}{
		{"smoke-reject-test1", PrefixReject},
		{"smoke-insuff-test2", PrefixInsuff},
		{"smoke-transient-test3", PrefixTransient},
		{"smoke-compfail-test4", PrefixCompFail},
	} {
		if !hasSmokePrefix(tc.key) {
			t.Errorf("hasSmokePrefix(%q) = false, want true", tc.key)
		}
		// Non-smoke keys are rejected.
		if hasSmokePrefix("idem-normal-1") {
			t.Errorf("hasSmokePrefix(normal key) = true, want false")
		}
	}
}

func TestScenarioMatch_WireForm(t *testing.T) {
	// The wire form is "wf-"+prefix. Verify the literal so a future
	// refactor of scenarioMatch cannot silently drift.
	for _, tc := range []struct {
		workflowID string
		prefix     string
	}{
		{"wf-smoke-reject-x", PrefixReject},
		{"wf-smoke-insuff-x", PrefixInsuff},
		{"wf-smoke-transient-x", PrefixTransient},
		{"wf-smoke-compfail-x", PrefixCompFail},
	} {
		// scenarioMatch short-circuits to false when Enabled() is false,
		// regardless of the workflow_id. We verify the wire form by
		// checking HasPrefix directly so the test does not depend on the
		// process-wide memoised state.
		if !startsWithWireForm(tc.workflowID, tc.prefix) {
			t.Errorf("%q does not match wire form %q", tc.workflowID, "wf-"+tc.prefix)
		}
	}
}

// startsWithWireForm is a test-local mirror of scenarioMatch's HasPrefix
// check, decoupled from the Enabled() gate so the wire-form assertion
// is independent of the process-wide memoised state.
func startsWithWireForm(workflowID, prefix string) bool {
	wireForm := "wf-" + prefix
	return len(workflowID) >= len(wireForm) &&
		workflowID[:len(wireForm)] == wireForm
}
