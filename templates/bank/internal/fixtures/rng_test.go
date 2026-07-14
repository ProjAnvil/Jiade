package fixtures

import "testing"

func TestRNG_Deterministic(t *testing.T) {
	g1 := NewRNG(42)
	g2 := NewRNG(42)
	for i := 0; i < 100; i++ {
		a, b := g1.IntRange(1, 1000), g2.IntRange(1, 1000)
		if a != b {
			t.Fatalf("第 %d 次: 同 seed 序列不一致 %d!=%d", i, a, b)
		}
	}
}

func TestRNG_DifferentSeedDiffers(t *testing.T) {
	if NewRNG(1).IntRange(0, 1<<30) == NewRNG(2).IntRange(0, 1<<30) {
		t.Error("不同 seed 应产生不同序列")
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
	t.Errorf("Choice 返回 %q 不在词库", got)
}
