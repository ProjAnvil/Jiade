package main

import (
	"bank_dcn/internal/gns"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mysqlx"
	"bank_dcn/internal/platform/redisx"
	"bank_dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	cache := redisx.Open(runx.MustEnv("REDIS_ADDR"))
	srv := gns.NewServer(db, cache)
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
