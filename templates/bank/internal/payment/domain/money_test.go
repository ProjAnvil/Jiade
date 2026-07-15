package domain

import (
	"os"
	"strings"
	"testing"
)

func TestMoneyRoundTrip(t *testing.T) {
	m := NewMoneyFromCents(123456)
	if m.String() != "1234.56" {
		t.Errorf("String=%s want 1234.56", m.String())
	}
	p, err := ParseCents("1234.56")
	if err != nil || p != m {
		t.Errorf("ParseCents round-trip failed: %v %v", p, err)
	}
}

// TestSourceHasNoFloat guards money.go source contains no float (financial no-float invariant).
func TestSourceHasNoFloat(t *testing.T) {
	b, err := os.ReadFile("money.go")
	if err != nil {
		t.Fatal("cannot read money.go:", err)
	}
	if strings.Contains(string(b), "float") {
		t.Fatal("money.go contains float, violating financial invariant")
	}
}
