package testfail

// Contract tests for the scenario matchers in the DISABLED state — the only
// state production ever sees (compose.yaml never sets
// BANK_TEST_FAILURES_ENABLED). Enabled() memoises via sync.Once, so the
// enabled branches are exercised by the smoke suite in test/smoke.sh rather
// than here; see testfail_test.go for the wire-form assertions.

import "testing"

func TestSmokeMatchers_DisabledStateIsInert(t *testing.T) {
	// t.Setenv guarantees the env var is unset for this test. NOTE:
	// Enabled() may already be memoised from an earlier test in this binary
	// — but with the variable unset in any fresh process the matchers below
	// are false, which is the production contract under test.
	t.Setenv("BANK_TEST_FAILURES_ENABLED", "")

	wireForms := []struct {
		match func(string) bool
		id    string
	}{
		{IsReject, "wf-smoke-reject-x"},
		{IsInsuff, "wf-smoke-insuff-x"},
		{IsTransient, "wf-smoke-transient-x"},
		{IsCompFail, "wf-smoke-compfail-x"},
	}
	for _, tc := range wireForms {
		if tc.match(tc.id) {
			t.Errorf("matcher must be false for %q when injection is disabled", tc.id)
		}
		// Non-smoke workflow ids are never matched either.
		if tc.match("wf-abc123") {
			t.Error("matcher must be false for a server-generated workflow id")
		}
	}
}

func TestWorkflowIDForSmoke_DisabledReturnsFalse(t *testing.T) {
	t.Setenv("BANK_TEST_FAILURES_ENABLED", "")
	id, ok := WorkflowIDForSmoke("smoke-reject-1")
	if ok || id != "" {
		t.Errorf("WorkflowIDForSmoke in disabled state = (%q, %v), want (\"\", false)", id, ok)
	}
}
