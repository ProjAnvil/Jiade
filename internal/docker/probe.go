// Package docker 探测 docker/compose/daemon 环境（up 命令前置）。
package docker

import (
	"context"
	"os/exec"
)

// Commander 执行命令的抽象（单测注入假实现）。
type Commander interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type realCommander struct{}

func (realCommander) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// ProbeResult docker 环境探测结果。
type ProbeResult struct {
	HasDocker     bool
	HasCompose    bool // docker compose 子命令可用
	DaemonRunning bool
}

// Probe 用真 exec 探测。
func Probe(ctx context.Context) ProbeResult {
	return ProbeWith(ctx, realCommander{})
}

// ProbeWith 用注入的 commander 探测（可单测）。
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

// OK up 前置：docker + compose + daemon 皆就绪。
func (p ProbeResult) OK() bool {
	return p.HasDocker && p.HasCompose && p.DaemonRunning
}

// Hint 失败时的人类可读提示。
func (p ProbeResult) Hint() string {
	switch {
	case !p.HasDocker:
		return "未检测到 docker，请先安装 Docker"
	case !p.HasCompose:
		return "未检测到 docker compose 子命令，请升级 Docker"
	case !p.DaemonRunning:
		return "docker daemon 未运行，请先启动 Docker Desktop"
	default:
		return ""
	}
}
