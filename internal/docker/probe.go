// Package docker detects the docker/compose/daemon environment (prefixed to the up command).
package docker

import (
	"context"
	"os/exec"
)

// Commander abstraction for executing commands (single test injects fake implementation).
type Commander interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type realCommander struct{}

func (realCommander) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// ProbeResult Docker environment detection result.
type ProbeResult struct {
	HasDocker     bool
	HasCompose    bool // docker compose subcommand is available
	DaemonRunning bool
}

// Probe probes with true exec.
func Probe(ctx context.Context) ProbeResult {
	return ProbeWith(ctx, realCommander{})
}

// ProbeWith probes with the injected commander (single testable).
func ProbeWith(ctx context.Context, cmd Commander) ProbeResult {
	res := ProbeResult{}
	if _, err := cmd.Output(ctx, "docker", "--version"); err != nil {
		return res
	}
	res.HasDocker = true
	if _, err := cmd.Output(ctx, "docker", "compose", "version"); err == nil {
		res.HasCompose = true
	}
	if _, err := cmd.Output(ctx, "docker", "info"); err == nil {
		res.DaemonRunning = true
	}
	return res
}

// OK up prerequisites: docker + compose + daemon are all ready.
func (p ProbeResult) OK() bool {
	return p.HasDocker && p.HasCompose && p.DaemonRunning
}

// Hint Human-readable hint on failure.
func (p ProbeResult) Hint() string {
	switch {
	case !p.HasDocker:
		return "Docker was not detected; install Docker first"
	case !p.HasCompose:
		return "The docker compose subcommand was not detected; upgrade Docker"
	case !p.DaemonRunning:
		return "The Docker daemon is not running; start Docker Desktop first"
	default:
		return ""
	}
}
