// Package ratelimit 实现每实例令牌桶限流中间件（仿真接入层职责）。
package ratelimit

import (
	"net/http"
	"sync"
	"time"
)

// Limiter 是容量等于 rps 的令牌桶。
type Limiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// New 创建每秒 rps 个令牌的限流器。
func New(rps float64) *Limiter {
	return newForTest(rps, time.Now)
}

func newForTest(rps float64, now func() time.Time) *Limiter {
	return &Limiter{rate: rps, tokens: rps, last: now(), now: now}
}

// Allow 取一个令牌，成功返回 true。
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

// Middleware 超限时返回 429。
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
