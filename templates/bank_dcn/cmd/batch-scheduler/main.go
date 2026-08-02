package main

import (
	"bank_dcn/internal/batch"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mysqlx"
	"bank_dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	srv := batch.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
