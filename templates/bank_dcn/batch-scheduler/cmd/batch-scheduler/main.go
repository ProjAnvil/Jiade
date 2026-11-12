package main

import (
	"bank-dcn/batch-scheduler/internal/batch"
	"bank-dcn/lib/metrics"
	"bank-dcn/lib/mysqlx"
	"bank-dcn/lib/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	srv := batch.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
