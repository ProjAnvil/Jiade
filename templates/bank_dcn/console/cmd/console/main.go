package main

import (
	"bank-dcn/console/internal/console"
	"bank-dcn/lib/metrics"
	"bank-dcn/lib/runx"
)

func main() {
	srv := console.NewServer(
		runx.Env("PROMETHEUS_URL", "http://prometheus:9090"),
		runx.Env("DOCKER_SOCKET", "/var/run/docker.sock"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
