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
		"docker --version":       true,
		"docker compose version": true,
		"docker info":            true,
	}}
	r := ProbeWith(context.Background(), cmd)
	if !r.OK() {
		t.Errorf("expected OK, got %+v hint=%q", r, r.Hint())
	}
}

func TestProbe_NoDocker(t *testing.T) {
	r := ProbeWith(context.Background(), fakeCmd{ok: map[string]bool{}})
	if r.HasDocker {
		t.Error("Docker should not be available")
	}
	if r.OK() {
		t.Error("the result should not be OK without Docker")
	}
	if !strings.Contains(r.Hint(), "install") {
		t.Errorf("the missing-Docker hint should contain 'install', got %q", r.Hint())
	}
}

func TestProbe_DaemonDown(t *testing.T) {
	cmd := fakeCmd{ok: map[string]bool{
		"docker --version":       true,
		"docker compose version": true,
	}}
	r := ProbeWith(context.Background(), cmd)
	if r.DaemonRunning {
		t.Error("the daemon should not be running")
	}
	if !strings.Contains(r.Hint(), "Docker Desktop") {
		t.Errorf("the stopped-daemon hint should contain 'Docker Desktop', got %q", r.Hint())
	}
}
