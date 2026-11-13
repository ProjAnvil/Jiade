package domain

import (
	"strings"
	"testing"
)

func TestParseCents(t *testing.T) {
	cases := []struct {
		in   string
		want Money
	}{
		{"1234.56", 123456},
		{"1234", 123400},
		{"1234.5", 123450},
		{"0.01", 1},
		{"0", 0},
		{"-1.50", -150},
	}
	for _, c := range cases {
		got, err := ParseCents(c.in)
		if err != nil {
			t.Fatalf("ParseCents(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseCents(%q)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseCents_TooManyDecimals(t *testing.T) {
	if _, err := ParseCents("1234.567"); err == nil {
		t.Error("more than 2 decimal places should error")
	}
}

func TestMoneyString(t *testing.T) {
	cases := []struct {
		m    Money
		want string
	}{
		{123456, "1234.56"},
		{123400, "1234.00"},
		{1, "0.01"},
		{0, "0.00"},
		{-150, "-1.50"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("Money(%d).String()=%q, want %q", c.m, got, c.want)
		}
	}
}

func TestMoneyRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00", "99.99", "1234567.89", "-0.50"} {
		m, err := ParseCents(s)
		if err != nil {
			t.Fatalf("ParseCents(%q): %v", s, err)
		}
		if got := m.String(); got != s {
			t.Errorf("round-trip: ParseCents(%q).String()=%q", s, got)
		}
	}
}

func TestMoneyAddSub(t *testing.T) {
	if got := (Money(100)).Add(Money(50)); got != 150 {
		t.Errorf("Add=%d, want 150", got)
	}
	if got := (Money(100)).Sub(Money(30)); got != 70 {
		t.Errorf("Sub=%d, want 70", got)
	}
}

// Prohibited float guard: The float32/float64 keyword must not appear in the source code (anti-regression).
func TestSourceHasNoFloat(t *testing.T) {
	src, err := readFile("money.go")
	if err != nil {
		t.Skip("readFile only available in test directory, skipping guard")
	}
	if strings.Contains(src, "float32") || strings.Contains(src, "float64") {
		t.Error("money.go must not use float")
	}
}

func readFile(name string) (string, error) {
	b, err := osReadFile(name)
	return string(b), err
}
