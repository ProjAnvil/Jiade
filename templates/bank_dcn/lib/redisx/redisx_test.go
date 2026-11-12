package redisx

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestOpen(t *testing.T) {
	addr := os.Getenv("DCN_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("DCN_TEST_REDIS_ADDR not set; skipping Redis integration test")
	}

	// Open retries for up to 60s, so probe reachability with a quick ping
	// first and only call Open once the server answers.
	probe := redis.NewClient(&redis.Options{Addr: addr})
	defer probe.Close()
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	if err := probe.Ping(probeCtx).Err(); err != nil {
		t.Skipf("redis not reachable (%v); skipping", err)
	}

	c := Open(addr)
	if c == nil {
		t.Fatal("Open returned nil")
	}
	defer c.Close()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		t.Fatalf("ping after Open: %v", err)
	}
}
