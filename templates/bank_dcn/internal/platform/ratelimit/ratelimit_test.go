package ratelimit

import (
	"testing"
	"time"
)

func TestLimiterAllowThenDeny(t *testing.T) {
	now := time.Unix(0, 0)
	l := newForTest(2, func() time.Time { return now })

	if !l.Allow() || !l.Allow() {
		t.Fatal("first two calls should be allowed")
	}
	if l.Allow() {
		t.Fatal("third call within same instant should be denied")
	}
	now = now.Add(600 * time.Millisecond) // refills 1.2 tokens
	if !l.Allow() {
		t.Fatal("call after refill should be allowed")
	}
}

func TestLimiterBurstCapped(t *testing.T) {
	now := time.Unix(0, 0)
	l := newForTest(1, func() time.Time { return now })
	now = now.Add(time.Hour) // after a long idle period the burst must not exceed the bucket capacity of 1
	if !l.Allow() {
		t.Fatal("first call should be allowed")
	}
	if l.Allow() {
		t.Fatal("burst should be capped at bucket size")
	}
}
