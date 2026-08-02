package dcnapp

import (
	"errors"
	"testing"
)

// classifyResult 把 applyMovement 的错误映射为 (回执状态, 原因, 是否基础设施错误)。
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
