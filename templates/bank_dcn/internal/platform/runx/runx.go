// Package runx provides small shared helpers for service startup.
package runx

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"os"
)

// Env reads an environment variable, falling back to def when unset.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// MustEnv reads a required environment variable, exiting if it is missing.
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env %s", key)
	}
	return v
}

// Serve starts the HTTP server (blocking, exits on error).
func Serve(addr string, h http.Handler) {
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, h))
}

// RandHex returns a 2n-character hex-encoded random string.
func RandHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
