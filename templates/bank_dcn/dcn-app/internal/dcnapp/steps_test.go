package dcnapp

import (
	"errors"
	"testing"
)

// classifyResult maps applyMovement errors to (receipt status, reason, is-infrastructure-error).
func TestClassifyResult(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		status     string
		infraError bool
	}{
		{"success", nil, "DONE", false},
		{"duplicate is idempotent DONE", errDuplicate, "DONE", false},
		{"insufficient is business FAILED", errInsufficient, "FAILED", false},
		{"unknown account is business FAILED", errNotFound, "FAILED", false},
		{"db error is infra (requeue)", errors.New("connection refused"), "", true},
	}
	for _, c := range cases {
		status, _, infra := classifyResult(c.err)
		if status != c.status || infra != c.infraError {
			t.Errorf("%s: classifyResult = (%q, infra=%v), want (%q, infra=%v)",
				c.name, status, infra, c.status, c.infraError)
		}
	}
}
