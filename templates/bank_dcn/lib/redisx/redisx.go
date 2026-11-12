// Package redisx wraps Redis connections with container startup retry.
package redisx

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Open establishes a Redis connection and pings it until reachable (waits up to 60s).
func Open(addr string) *redis.Client {
	c := redis.NewClient(&redis.Options{Addr: addr})
	for i := 0; i < 60; i++ {
		if err := c.Ping(context.Background()).Err(); err == nil {
			return c
		}
		time.Sleep(time.Second)
	}
	log.Fatalf("redis not reachable: %s", addr)
	return nil
}
