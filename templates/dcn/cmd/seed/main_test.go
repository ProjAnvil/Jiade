package main

import (
	"math/rand"
	"testing"
)

func TestPersonNameDeterministic(t *testing.T) {
	a := personName(rand.New(rand.NewSource(42)))
	b := personName(rand.New(rand.NewSource(42)))
	if a != b || a == "" {
		t.Fatalf("name not deterministic or empty: %q vs %q", a, b)
	}
}

func TestInitialBalance(t *testing.T) {
	// 每单元前 2 户固定 1000.00（verify 与 README 示例依赖）
	for _, i := range []int{0, 1} {
		if got := initialBalance(rand.New(rand.NewSource(1)), 1000, i); got != "1000.00" {
			t.Fatalf("fixed account %d balance = %s, want 1000.00", i, got)
		}
	}
	// 其余确定性随机，且在 [100, 100000] 区间
	r1, r2 := rand.New(rand.NewSource(7)), rand.New(rand.NewSource(7))
	for i := 2; i < 50; i++ {
		b1, b2 := initialBalance(r1, 1000, i), initialBalance(r2, 1000, i)
		if b1 != b2 {
			t.Fatalf("non-deterministic balance at %d: %s vs %s", i, b1, b2)
		}
	}
}
