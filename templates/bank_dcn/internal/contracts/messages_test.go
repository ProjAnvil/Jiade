package contracts

import "testing"

func TestStepDirection(t *testing.T) {
	cases := []struct {
		action, suffix, dir string
		ok                  bool
	}{
		{"DEBIT", "", "DEBIT", true},
		{"CREDIT", "", "CREDIT", true},
		{"COMPENSATE_DEBIT", ":comp", "CREDIT", true},
		{"COMPENSATE_CREDIT", ":comp", "DEBIT", true},
		{"BOGUS", "", "", false},
	}
	for _, c := range cases {
		suffix, dir, ok := StepDirection(c.action)
		if suffix != c.suffix || dir != c.dir || ok != c.ok {
			t.Errorf("StepDirection(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.action, suffix, dir, ok, c.suffix, c.dir, c.ok)
		}
	}
}

func TestReverseAction(t *testing.T) {
	if got, ok := ReverseAction("DEBIT"); !ok || got != "COMPENSATE_DEBIT" {
		t.Errorf("ReverseAction(DEBIT) = %q,%v", got, ok)
	}
	if got, ok := ReverseAction("CREDIT"); !ok || got != "COMPENSATE_CREDIT" {
		t.Errorf("ReverseAction(CREDIT) = %q,%v", got, ok)
	}
	if _, ok := ReverseAction("COMPENSATE_DEBIT"); ok {
		t.Error("ReverseAction(COMPENSATE_DEBIT) should not be ok")
	}
}
