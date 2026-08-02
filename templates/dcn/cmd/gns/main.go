package main

import (
	"dcn/internal/gns"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/redisx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	cache := redisx.Open(runx.MustEnv("REDIS_ADDR"))
	srv := gns.NewServer(db, cache)
	runx.Serve(":"+runx.Env("PORT", "8080"), srv.Handler())
}
