package seed

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestParseScale(t *testing.T) {
	for _, valid := range []string{"dev", "demo", "load"} {
		scale, err := ParseScale(valid)
		if err != nil || string(scale) != valid {
			t.Errorf("ParseScale(%q)=%q err=%v, want accepted", valid, scale, err)
		}
	}
	for _, invalid := range []string{"", "Dev", "production", "dev "} {
		if _, err := ParseScale(invalid); err == nil ||
			!strings.Contains(err.Error(), "unknown scale") {
			t.Errorf("ParseScale(%q) err=%v, want unknown scale rejection", invalid, err)
		}
	}
}

func TestCountsForEveryScale(t *testing.T) {
	tests := []struct {
		scale     Scale
		wantItems int
		wantOnes  int
	}{
		{Dev, 80, 100},
		{Demo, 1_500, 10_000},
		{Load, 12_000, 1_000_000},
	}
	for _, test := range tests {
		t.Run(string(test.scale), func(t *testing.T) {
			counts := countsFor(test.scale)
			if counts.Products != test.wantItems || counts.Orders != test.wantOnes {
				t.Fatalf("counts=%+v, want products=%d orders=%d", counts, test.wantItems, test.wantOnes)
			}
			if counts.Customers <= 0 || counts.Locations <= 0 || counts.Brands <= 0 ||
				counts.Categories <= 0 || counts.CartsPerOrder != 1 {
				t.Fatalf("counts=%+v, want positive domain counts", counts)
			}
		})
	}
	if got := countsFor(Scale("bogus")); got != (counts{}) {
		t.Fatalf("unknown scale counts=%+v, want zero counts", got)
	}
}

func TestConfigValidateRejectsBadInput(t *testing.T) {
	if err := (Config{Scale: Dev, Seed: 1}).validate(); err != nil {
		t.Fatalf("valid config error: %v", err)
	}
	if err := (Config{Scale: Scale("huge"), Seed: 1}).validate(); err == nil ||
		!strings.Contains(err.Error(), "unknown scale") {
		t.Fatalf("unknown scale error=%v", err)
	}
	if err := (Config{Scale: Dev, Seed: -1}).validate(); err == nil ||
		!strings.Contains(err.Error(), "negative seed") {
		t.Fatalf("negative seed error=%v", err)
	}
}

func TestGenerateRejectsInvalidConfig(t *testing.T) {
	if _, err := Generate(Config{Scale: Scale("nope"), Seed: 1}); err == nil {
		t.Fatal("Generate must reject unknown scales before allocating")
	}
	if _, err := Generate(Config{Scale: Dev, Seed: -5}); err == nil {
		t.Fatal("Generate must reject negative seeds")
	}
}

func TestGenerationAnchorIsStableAndBounded(t *testing.T) {
	epoch := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
	if anchor := generationAnchor(Config{Scale: Dev, Seed: 0}); !anchor.Equal(epoch) {
		t.Fatalf("seed 0 anchor=%v, want epoch", anchor)
	}
	same := generationAnchor(Config{Scale: Dev, Seed: 42})
	if same != generationAnchor(Config{Scale: Demo, Seed: 42}) {
		t.Fatal("anchor must depend only on the seed modulo its window")
	}
	// A seed beyond one day wraps within the 86400-second window.
	if got := generationAnchor(Config{Scale: Dev, Seed: 86400 + 7}); !got.Equal(epoch.Add(7 * time.Second)) {
		t.Fatalf("wrapped anchor=%v, want epoch+7s", got)
	}
}

func TestRefundAmountFor(t *testing.T) {
	if got := refundAmountFor("refunded", 10_000); got != 10_000 {
		t.Fatalf("full refund=%d, want intent amount", got)
	}
	if got := refundAmountFor("partially_refunded", 10_001); got != 5_000 {
		t.Fatalf("partial refund=%d, want floor(10001/2)=5000", got)
	}
	// A tiny captured amount still refunds at least one minor unit.
	if got := refundAmountFor("partially_refunded", 1); got != 1 {
		t.Fatalf("minimum refund=%d, want 1", got)
	}
}

func TestPaymentIntentStatusMapping(t *testing.T) {
	tests := []struct {
		orderStatus string
		want        string
	}{
		{"pending", "requires_method"},
		{"authorized", "authorized"},
		{"paid", "succeeded"},
		{"failed", "failed"},
		{"partially_refunded", "partially_refunded"},
		{"refunded", "partially_refunded"},
		{"unknown", "requires_method"},
	}
	for _, test := range tests {
		if got := paymentIntentStatus(test.orderStatus); got != test.want {
			t.Errorf("paymentIntentStatus(%q)=%q, want %q", test.orderStatus, got, test.want)
		}
	}
}

func TestZipfIndexBounds(t *testing.T) {
	if got := zipfIndex(rand.New(rand.NewSource(1)), 0); got != 0 {
		t.Fatalf("zipfIndex(size=0)=%d, want 0", got)
	}
	stream := rand.New(rand.NewSource(7))
	for index := 0; index < 100; index++ {
		if got := zipfIndex(stream, 10); got < 0 || got >= 10 {
			t.Fatalf("zipfIndex=%d out of [0,10)", got)
		}
	}
}

func TestMinorRangeClampsNegativeMinimum(t *testing.T) {
	stream := rand.New(rand.NewSource(3))
	for index := 0; index < 50; index++ {
		if got := minorRange(stream, -100, 50); got < 0 {
			t.Fatalf("minorRange=%d, want non-negative after clamping", got)
		}
	}
}
