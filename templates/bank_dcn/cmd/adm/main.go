package main

import (
	"bank_dcn/internal/adm"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mq"
	"bank_dcn/internal/platform/mysqlx"
	"bank_dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	srv := adm.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	srv.DeclareAndConsume(mqc)
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
