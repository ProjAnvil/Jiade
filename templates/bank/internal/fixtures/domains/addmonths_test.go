package domains

import "testing"

func TestAddMonths(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"2025-06-15", 12, "2026-06-15"},
		{"2025-06-15", 24, "2027-06-15"},
		{"2025-06-15", 1, "2025-07-15"},
		{"2026-07-13", -1, "2026-06-13"},
		{"2026-07-13", -2, "2026-05-13"},
		{"bad-date", 3, "bad-date"}, // If parsing fails, return as is
	}
	for _, c := range cases {
		if got := addMonths(c.in, c.n); got != c.want {
			t.Errorf("addMonths(%s,%d)=%s want %s", c.in, c.n, got, c.want)
		}
	}
}
