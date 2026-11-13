package fixtures

import "testing"

func TestRNG_Deterministic(t *testing.T) {
	g1 := NewRNG(42)
	g2 := NewRNG(42)
	for i := 0; i < 100; i++ {
		a, b := g1.IntRange(1, 1000), g2.IntRange(1, 1000)
		if a != b {
			t.Fatalf("iteration %d: same seed produced different sequences %d!=%d", i, a, b)
		}
	}
}

func TestRNG_DifferentSeedDiffers(t *testing.T) {
	if NewRNG(1).IntRange(0, 1<<30) == NewRNG(2).IntRange(0, 1<<30) {
		t.Error("different seeds should produce different sequences")
	}
}

func TestChoice(t *testing.T) {
	g := NewRNG(42)
	got := g.Choice(Branches)
	for _, b := range Branches {
		if b == got {
			return
		}
	}
	t.Errorf("Choice returned %q not in wordlist", got)
}

func TestRNG_Float64Deterministic(t *testing.T) {
	a := NewRNG(99).Float64()
	b := NewRNG(99).Float64()
	if a != b {
		t.Errorf("Float64 not deterministic: %v != %v", a, b)
	}
	if a < 0 || a >= 1 {
		t.Errorf("Float64 out of range: %v", a)
	}
}

func TestScaleFactor(t *testing.T) {
	if ScaleFactor(ScaleDev) != 0.25 {
		t.Error("dev should be 0.25")
	}
	if ScaleFactor(ScaleFull) != 1.0 {
		t.Error("full should be 1.0")
	}
}
