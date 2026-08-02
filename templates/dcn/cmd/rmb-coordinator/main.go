package main

import (
	"strconv"
	"time"

	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
	"dcn/internal/rmb"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	secs, err := strconv.Atoi(runx.Env("TX_TIMEOUT_SECONDS", "5"))
	if err != nil || secs <= 0 {
		secs = 5
	}
	coord := rmb.NewCoordinator(db, mqc, time.Duration(secs)*time.Second)
	coord.Run()
	runx.Serve(":"+runx.Env("PORT", "8080"), coord.Handler())
}
