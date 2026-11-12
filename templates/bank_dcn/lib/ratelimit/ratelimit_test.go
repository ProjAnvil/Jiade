package ratelimit

import (
	"net/http"
	"net/http/httptest"
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

func TestNewAllowsFirstCall(t *testing.T) {
	l := New(10)
	if !l.Allow() {
		t.Fatal("first call on a fresh limiter should be allowed")
	}
}

func TestMiddlewarePassesAllowedRequestToNext(t *testing.T) {
	now := time.Unix(0, 0)
	l := newForTest(2, func() time.Time { return now })

	var hits int
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if hits != 1 {
		t.Fatalf("next handler hits = %d, want 1", hits)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestMiddlewareReturns429WhenExhausted(t *testing.T) {
	now := time.Unix(0, 0)
	l := newForTest(1, func() time.Time { return now })

	var hits int
	h := l.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))

	// First request consumes the only token.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if hits != 1 {
		t.Fatalf("next handler hits = %d, want 1 (second call must be blocked)", hits)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	if got, want := rec.Body.String(), `{"error":"rate limited"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
