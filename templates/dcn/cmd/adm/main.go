package main

import (
	"dcn/internal/adm"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	srv := adm.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	srv.DeclareAndConsume(mqc)
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
