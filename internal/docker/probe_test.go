package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCmd struct {
	ok map[string]bool // key = "name arg1 arg2 ..."
}

func (f fakeCmd) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	if f.ok[key] {
		return []byte("ok"), nil
	}
	return nil, errors.New("exec: not found")
}

func TestProbe_AllPresent(t *testing.T) {
	cmd := fakeCmd{ok: map[string]bool{
		"docker --version":     true,
		"docker compose version": true,
		"docker info":          true,
	}}
	r := ProbeWith(context.Background(), cmd)
	if !r.OK() {
		t.Errorf("应 OK, got %+v hint=%q", r, r.Hint())
	}
}

func TestProbe_NoDocker(t *testing.T) {
	r := ProbeWith(context.Background(), fakeCmd{ok: map[string]bool{}})
	if r.HasDocker {
		t.Error("不应有 docker")
	}
	if r.OK() {
		t.Error("无 docker 不应 OK")
	}
	if !strings.Contains(r.Hint(), "安装") {
		t.Errorf("无 docker 提示应含'安装', got %q", r.Hint())
	}
}

func TestProbe_DaemonDown(t *testing.T) {
	cmd := fakeCmd{ok: map[string]bool{
		"docker --version":       true,
		"docker compose version": true,
	}}
	r := ProbeWith(context.Background(), cmd)
	if r.DaemonRunning {
		t.Error("daemon 不应运行")
	}
	if !strings.Contains(r.Hint(), "Docker Desktop") {
		t.Errorf("daemon 未运行提示应含'Docker Desktop', got %q", r.Hint())
	}
}
