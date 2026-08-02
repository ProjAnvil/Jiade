package main

import (
	"math/rand"
	"regexp"
	"strconv"
	"testing"
)

func TestPersonNameDeterministic(t *testing.T) {
	a := personName(rand.New(rand.NewSource(42)))
	b := personName(rand.New(rand.NewSource(42)))
	if a != b || a == "" {
		t.Fatalf("name not deterministic or empty: %q vs %q", a, b)
	}
}

var twoDecimals = regexp.MustCompile(`^\d+\.\d{2}$`)

func TestInitialBalance(t *testing.T) {
	// The first 2 accounts of each unit are fixed at 1000.00 (depended on by verify and the README examples)
	for _, i := range []int{0, 1} {
		if got := initialBalance(rand.New(rand.NewSource(1)), i); got != "1000.00" {
			t.Fatalf("fixed account %d balance = %s, want 1000.00", i, got)
		}
	}
	// The rest are deterministic-random, within [100.00, 100000.00], with 2 decimal places
	r1, r2 := rand.New(rand.NewSource(7)), rand.New(rand.NewSource(7))
	for i := 2; i < 50; i++ {
		b1, b2 := initialBalance(r1, i), initialBalance(r2, i)
		if b1 != b2 {
			t.Fatalf("non-deterministic balance at %d: %s vs %s", i, b1, b2)
		}
		if !twoDecimals.MatchString(b1) {
			t.Fatalf("balance at %d not 2-decimal: %q", i, b1)
		}
		v, err := strconv.ParseFloat(b1, 64)
		if err != nil {
			t.Fatalf("balance at %d not parseable: %q: %v", i, b1, err)
		}
		if v < 100.00 || v > 100000.00 {
			t.Fatalf("balance at %d out of [100.00, 100000.00]: %s", i, b1)
		}
	}
}
