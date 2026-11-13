package client

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 3, OpenFor: time.Second})
	for range 3 {
		breaker.Record(false)
	}
	if err := breaker.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("Allow() = %v, want ErrCircuitOpen", err)
	}
}

func TestBreakerSuccessResetsConsecutiveFailures(t *testing.T) {
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 3, OpenFor: time.Second})
	breaker.Record(false)
	breaker.Record(false)
	breaker.Record(true)
	breaker.Record(false)
	if err := breaker.Allow(); err != nil {
		t.Fatalf("Allow() = %v, want nil", err)
	}
}

func TestBreakerAllowsSingleHalfOpenProbe(t *testing.T) {
	now := time.Now()
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 1, OpenFor: time.Second, Now: func() time.Time { return now }})
	breaker.Record(false)
	now = now.Add(time.Second)
	if err := breaker.Allow(); err != nil {
		t.Fatalf("first half-open Allow() = %v, want nil", err)
	}
	if err := breaker.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("second half-open Allow() = %v, want ErrCircuitOpen", err)
	}
	breaker.Record(true)
	if err := breaker.Allow(); err != nil {
		t.Fatalf("closed Allow() = %v, want nil", err)
	}
}

func TestBreakerAllowsExactlyOneConcurrentHalfOpenProbe(t *testing.T) {
	now := time.Now()
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 1, OpenFor: time.Second, Now: func() time.Time { return now }})
	breaker.Record(false)
	now = now.Add(time.Second)

	const contenders = 32
	start := make(chan struct{})
	results := make(chan error, contenders)
	var group sync.WaitGroup
	for range contenders {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- breaker.Allow()
		}()
	}
	close(start)
	group.Wait()
	close(results)

	allowed := 0
	for err := range results {
		if err == nil {
			allowed++
			continue
		}
		if !errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("Allow() error=%v, want ErrCircuitOpen", err)
		}
	}
	if allowed != 1 {
		t.Fatalf("allowed contenders=%d, want exactly 1", allowed)
	}
}

func TestBreakerIsSafeForConcurrentUse(t *testing.T) {
	breaker := NewBreaker(BreakerConfig{FailureThreshold: 1000, OpenFor: time.Second})
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			for range 100 {
				_ = breaker.Allow()
				breaker.Record(false)
				breaker.Record(true)
			}
		}()
	}
	group.Wait()
}
