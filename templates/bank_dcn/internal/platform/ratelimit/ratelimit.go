// Package ratelimit implements a per-instance token-bucket rate-limiting middleware (simulating the access layer's role).
package ratelimit

import (
	"net/http"
	"sync"
	"time"
)

// Limiter is a token bucket whose capacity equals rps.
type Limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// New creates a limiter that issues rps tokens per second.
func New(rps float64) *Limiter {
	return newForTest(rps, time.Now)
}

func newForTest(rps float64, now func() time.Time) *Limiter {
	return &Limiter{rate: rps, tokens: rps, last: now(), now: now}
}

// Allow takes one token, returning true on success.
func (l *Limiter) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens += now.Sub(l.last).Seconds() * l.rate
	if l.tokens > l.rate {
		l.tokens = l.rate
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// Middleware returns 429 when the rate limit is exceeded.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
