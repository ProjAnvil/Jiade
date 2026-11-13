package fixtures

// Tests for RandomDate's edge cases: malformed bounds pass through, a
// reversed range collapses to the start date, and valid ranges stay inside
// [start, end].

import (
	"testing"
	"time"
)

func TestRandomDate_WithinRange(t *testing.T) {
	rng := NewRNG(42)
	start, end := "2026-07-01", "2026-07-31"
	t0, _ := time.Parse("2006-01-02", start)
	t1, _ := time.Parse("2006-01-02", end)
	for i := 0; i < 50; i++ {
		got := RandomDate(rng, start, end)
		d, err := time.Parse("2006-01-02", got)
		if err != nil {
			t.Fatalf("RandomDate returned %q: %v", got, err)
		}
		if d.Before(t0) || d.After(t1) {
			t.Fatalf("RandomDate = %q, outside [%s,%s]", got, start, end)
		}
	}
}

func TestRandomDate_SingleDay(t *testing.T) {
	rng := NewRNG(7)
	if got := RandomDate(rng, "2026-07-01", "2026-07-01"); got != "2026-07-01" {
		t.Errorf("single-day range must return that day, got %q", got)
	}
}

func TestRandomDate_ReversedRangeClampsToStart(t *testing.T) {
	rng := NewRNG(7)
	if got := RandomDate(rng, "2026-07-31", "2026-07-01"); got != "2026-07-31" {
		t.Errorf("reversed range must collapse to start, got %q", got)
	}
}

func TestRandomDate_MalformedBoundsPassThrough(t *testing.T) {
	rng := NewRNG(7)
	if got := RandomDate(rng, "not-a-date", "2026-07-01"); got != "not-a-date" {
		t.Errorf("malformed start must pass through, got %q", got)
	}
	if got := RandomDate(rng, "2026-07-01", "oops"); got != "oops" {
		t.Errorf("malformed end must pass through, got %q", got)
	}
}
