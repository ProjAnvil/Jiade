package domains

// Direct tests for the small pure helpers the writers rely on: NULL
// marshalling, money comparison, placeholder generation, and pick helpers.

import (
	"testing"

	"bank/loan/loan/domain"
	"bank/seed/internal/fixtures"
)

func TestNullable(t *testing.T) {
	if nullable("") != nil {
		t.Error("empty string must map to NULL (nil)")
	}
	if got := nullable("x"); got != "x" {
		t.Errorf("non-empty string must pass through, got %#v", got)
	}
}

func TestMinMoney(t *testing.T) {
	if got := minMoney(domain.NewMoneyFromCents(100), domain.NewMoneyFromCents(50)); got.Cents() != 50 {
		t.Errorf("minMoney(100,50) = %d, want 50", got.Cents())
	}
	if got := minMoney(domain.NewMoneyFromCents(-1), domain.NewMoneyFromCents(0)); got.Cents() != -1 {
		t.Errorf("minMoney must handle negatives, got %d", got.Cents())
	}
}

func TestPlaceholders(t *testing.T) {
	tests := []struct {
		rows, cols int
		want       string
	}{
		{1, 1, "($1)"},
		{1, 2, "($1,$2)"},
		{2, 2, "($1,$2),($3,$4)"},
		{3, 1, "($1),($2),($3)"},
	}
	for _, tc := range tests {
		if got := placeholders(tc.rows, tc.cols); got != tc.want {
			t.Errorf("placeholders(%d,%d) = %q, want %q", tc.rows, tc.cols, got, tc.want)
		}
	}
}

func TestMaxIntMinInt(t *testing.T) {
	if maxInt(3, 7) != 7 || maxInt(7, 3) != 7 {
		t.Error("maxInt wrong")
	}
	if minInt(3, 7) != 3 || minInt(7, 3) != 3 {
		t.Error("minInt wrong")
	}
}

func TestPickStr_EmptyList(t *testing.T) {
	rng := fixtures.NewRNG(1)
	if got := pickStr(rng, nil); got != "" {
		t.Errorf("pickStr(nil) = %q, want empty", got)
	}
	if got := pickStr(rng, []string{"only"}); got != "only" {
		t.Errorf("pickStr(single) = %q, want only", got)
	}
}

func TestOrderVolumeForDay(t *testing.T) {
	// volume must scale linearly with both scale factor and day factor.
	a := orderVolumeForDay(1.0, 1.0)
	b := orderVolumeForDay(2.0, 1.0)
	c := orderVolumeForDay(1.0, 2.0)
	if a <= 0 {
		t.Fatalf("baseline volume must be positive, got %d", a)
	}
	if b < a || c < a {
		t.Errorf("volume must be non-decreasing in both factors: %d %d %d", a, b, c)
	}
}
