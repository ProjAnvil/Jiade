package main

import (
	"bank-dcn/adm/internal/adm"
	"bank-dcn/lib/metrics"
	"bank-dcn/lib/mq"
	"bank-dcn/lib/mysqlx"
	"bank-dcn/lib/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	srv := adm.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	srv.DeclareAndConsume(mqc)
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
