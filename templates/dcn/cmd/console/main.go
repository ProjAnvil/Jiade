package main

import (
	"dcn/internal/console"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/runx"
)

func main() {
	srv := console.NewServer(
		runx.Env("PROMETHEUS_URL", "http://prometheus:9090"),
		runx.Env("DOCKER_SOCKET", "/var/run/docker.sock"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount(srv.Handler()))
}
