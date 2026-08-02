package main

import (
	"dcn/internal/batch"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	srv := batch.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
