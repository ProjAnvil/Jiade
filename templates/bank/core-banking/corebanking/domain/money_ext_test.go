package domain

// Table tests for Money arithmetic and the ParseCents edge cases that the
// seed and repo layers rely on (negative amounts, single-digit fractions,
// NULL/empty passthrough, malformed input).

import "testing"

func TestMoneyArithmetic(t *testing.T) {
	a := NewMoneyFromCents(10025)
	b := NewMoneyFromCents(-25)
	if got := a.Add(b); got.Cents() != 10000 {
		t.Errorf("Add = %d, want 10000", got.Cents())
	}
	if got := a.Sub(b); got.Cents() != 10050 {
		t.Errorf("Sub = %d, want 10050", got.Cents())
	}
	if NewMoneyFromCents(-1).Cents() != -1 {
		t.Errorf("Cents round trip failed for negative")
	}
}

func TestParseCents_Table(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"12.34", 1234},
		{"12.3", 1230},  // single fraction digit is padded
		{"12.", 1200},   // empty fraction is padded
		{".99", 99},     // empty integer part
		{"-12.34", -1234},
		{" 12.34 ", 1234}, // surrounding whitespace trimmed
		{"", 0},           // NULL NUMERIC scans as ""
	}
	for _, tc := range tests {
		got, err := ParseCents(tc.in)
		if err != nil {
			t.Errorf("ParseCents(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got.Cents() != tc.want {
			t.Errorf("ParseCents(%q) = %d, want %d", tc.in, got.Cents(), tc.want)
		}
	}
}

func TestParseCents_Errors(t *testing.T) {
	for _, in := range []string{"12.345", "abc", "1.2.3"} {
		if _, err := ParseCents(in); err == nil {
			t.Errorf("ParseCents(%q) should fail", in)
		}
	}
}

func TestMoneyString_Table(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0.00"},
		{5, "0.05"},
		{123456, "1234.56"},
		{-123456, "-1234.56"},
		{-5, "-0.05"},
	}
	for _, tc := range tests {
		if got := NewMoneyFromCents(tc.in).String(); got != tc.want {
			t.Errorf("Money(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
