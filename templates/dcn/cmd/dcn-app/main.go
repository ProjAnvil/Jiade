package main

import (
	"strconv"

	"github.com/shopspring/decimal"

	"dcn/internal/dcnapp"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	dcnID := runx.MustEnv("DCN_ID")
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	rps, err := strconv.ParseFloat(runx.Env("RATE_LIMIT_RPS", "200"), 64)
	if err != nil {
		rps = 200
	}
	rate, err := decimal.NewFromString(runx.Env("INTEREST_DAILY_RATE", "0.0001"))
	if err != nil || rate.IsNegative() {
		rate = decimal.RequireFromString("0.0001")
	}
	srv := dcnapp.NewServer(dcnID, db,
		runx.MustEnv("GNS_ENDPOINT"), runx.MustEnv("RMB_ENDPOINT"), mqc, rps, rate)
	srv.DeclareAndConsume()
	runx.Serve(":"+runx.Env("PORT", "8080"), srv.Handler())
}
