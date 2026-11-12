package main

import (
	"bank-dcn/gns/internal/gns"
	"bank-dcn/lib/metrics"
	"bank-dcn/lib/mysqlx"
	"bank-dcn/lib/redisx"
	"bank-dcn/lib/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	cache := redisx.Open(runx.MustEnv("REDIS_ADDR"))
	srv := gns.NewServer(db, cache)
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
