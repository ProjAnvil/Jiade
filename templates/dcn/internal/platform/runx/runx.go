// Package runx 提供服务启动的公共小工具。
package runx

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
)

// Env 读环境变量，缺省返回 def。
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// MustEnv 读必需环境变量，缺失即退出。
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env %s", key)
	}
	return v
}

// Serve 启动 HTTP 服务（阻塞，出错即退出）。
func Serve(addr string, h http.Handler) {
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}

// RandHex 返回 2n 位十六进制随机串。
func RandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
