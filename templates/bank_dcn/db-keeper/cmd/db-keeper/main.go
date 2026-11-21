// Command db-keeper is the arbitration and failover service for the
// self-managed MySQL sets. Three keeper nodes form a raft group (majority
// quorum); only the leader probes backends, decides failovers, and executes
// the switch procedure: wait for replica catch-up -> fence the old primary
// (super_read_only) -> promote the replica -> re-attach the old primary as
// the new replica -> repoint DBProxy routing. DBAdmin talks to the HTTP API.
package main

import (
	"log"
	"net/http"
	"time"

	_ "github.com/go-sql-driver/mysql" // register the mysql driver for database/sql

	"bank-dcn/db-keeper/internal/keeper"
	"bank-dcn/lib/metrics"
	"bank-dcn/lib/runx"
)

func main() {
	k, err := keeper.New(keeper.Config{
		NodeID:   runx.MustEnv("NODE_ID"),
		RaftAddr: runx.Env("RAFT_ADDR", ":18111"),
		Peers:    runx.MustEnv("PEERS"),
		SetsSpec: runx.MustEnv("SETS"),
		RootPW:   runx.Env("MYSQL_ROOT_PASSWORD", "dcn123"),
		Proxies:  runx.Env("PROXIES", "http://db-proxy:18120"),
	})
	if err != nil {
		log.Fatalf("keeper: %v", err)
	}

	go k.RunActuator()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	metrics.Handle(mux, "db-keeper", "GET /state", http.HandlerFunc(k.HandleState))
	metrics.Handle(mux, "db-keeper", "POST /sets/{name}/switch", http.HandlerFunc(k.HandleSwitch))
	runx.Serve(":"+runx.Env("PORT", "18110"), metrics.Mount(mux))
	_ = time.Now()
}
