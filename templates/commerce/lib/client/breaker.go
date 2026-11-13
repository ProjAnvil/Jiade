package client

import (
	"errors"
	"sync"
	"time"
)

// ErrCircuitOpen reports that an upstream is temporarily unavailable because
// its circuit breaker is open.
var ErrCircuitOpen = errors.New("client circuit breaker open")

// BreakerConfig controls a circuit breaker. Now is injectable for deterministic
// tests; production callers normally leave it nil.
type BreakerConfig struct {
	FailureThreshold int
	OpenFor          time.Duration
	Now              func() time.Time
}

// Breaker stops calls to an unhealthy upstream and permits one half-open probe
// after the configured cooling-off period.
type Breaker struct {
	mu       sync.Mutex
	config   BreakerConfig
	state    breakerState
	failures int
	openedTo time.Time
	probing  bool
}

type breakerState uint8

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

// NewBreaker creates a concurrency-safe circuit breaker.
func NewBreaker(config BreakerConfig) *Breaker {
	if config.FailureThreshold <= 0 {
		config.FailureThreshold = 5
	}
	if config.OpenFor <= 0 {
		config.OpenFor = 30 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Breaker{config: config}
}

// Allow reports whether a request may proceed. When open, the first caller
// after the cooling-off period becomes the sole half-open probe.
func (breaker *Breaker) Allow() error {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	if breaker.state == breakerClosed {
		return nil
	}
	if breaker.state == breakerOpen && !breaker.config.Now().Before(breaker.openedTo) {
		breaker.state = breakerHalfOpen
	}
	if breaker.state == breakerHalfOpen && !breaker.probing {
		breaker.probing = true
		return nil
	}
	return ErrCircuitOpen
}

// Record records the outcome of an allowed request.
func (breaker *Breaker) Record(success bool) {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	switch breaker.state {
	case breakerClosed:
		if success {
			breaker.failures = 0
			return
		}
		breaker.failures++
		if breaker.failures >= breaker.config.FailureThreshold {
			breaker.state = breakerOpen
			breaker.openedTo = breaker.config.Now().Add(breaker.config.OpenFor)
		}
	case breakerHalfOpen:
		breaker.probing = false
		if success {
			breaker.state = breakerClosed
			breaker.failures = 0
			return
		}
		breaker.state = breakerOpen
		breaker.openedTo = breaker.config.Now().Add(breaker.config.OpenFor)
	}
}

// Abandon releases a half-open probe that ended because the caller's context
// was canceled or expired. It intentionally does not count as an upstream
// failure or reset the breaker.
func (breaker *Breaker) Abandon() {
	breaker.mu.Lock()
	defer breaker.mu.Unlock()

	if breaker.state == breakerHalfOpen {
		breaker.probing = false
	}
}
