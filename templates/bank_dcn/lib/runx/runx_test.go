package runx

import (
	"encoding/hex"
	"testing"
)

func TestEnv(t *testing.T) {
	t.Run("set returns value", func(t *testing.T) {
		t.Setenv("RUNX_TEST_SET", "hello")
		if got := Env("RUNX_TEST_SET", "fallback"); got != "hello" {
			t.Fatalf("Env = %q, want hello", got)
		}
	})

	t.Run("unset returns default", func(t *testing.T) {
		// Env treats an empty value the same as unset.
		t.Setenv("RUNX_TEST_UNSET", "")
		if got := Env("RUNX_TEST_UNSET", "fallback"); got != "fallback" {
			t.Fatalf("Env = %q, want fallback", got)
		}
	})
}

func TestMustEnv(t *testing.T) {
	t.Setenv("RUNX_TEST_REQUIRED", "abc")
	if got := MustEnv("RUNX_TEST_REQUIRED"); got != "abc" {
		t.Fatalf("MustEnv = %q, want abc", got)
	}
	// The missing-variable path calls log.Fatalf (process exit), so it is not
	// unit-testable.
}

func TestRandHex(t *testing.T) {
	s := RandHex(8)
	if len(s) != 16 {
		t.Fatalf("len = %d, want 16 (two hex chars per byte)", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		t.Fatalf("not valid hex: %v", err)
	}
	if RandHex(8) == s {
		t.Fatal("two calls should produce different values")
	}
}
