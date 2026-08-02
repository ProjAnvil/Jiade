// Package redisx 封装 Redis 连接（带容器启动等待重试）。
package redisx

import (
	"context"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Open 建立并 ping 通 Redis 连接（最多等待 60s）。
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
