package console

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NewServer wires the Prometheus URL plus an HTTP client and a Docker client
// that dials the given unix socket, both with 5s timeouts.
func TestNewServerDefaults(t *testing.T) {
	s := NewServer("http://prom:9090", "/tmp/docker.sock")
	if s.prom != "http://prom:9090" {
		t.Fatalf("prom = %q", s.prom)
	}
	if s.hc.Timeout != 5*time.Second {
		t.Fatalf("hc timeout = %v, want 5s", s.hc.Timeout)
	}
	if s.docker.Timeout != 5*time.Second {
		t.Fatalf("docker timeout = %v, want 5s", s.docker.Timeout)
	}
}

// Handler routing: every registered route reaches its handler; unmatched
// methods and paths fall through to the mux defaults (405/404).
func TestHandlerRouting(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success"})
	}))
	defer prom.Close()
	h := NewServer(prom.URL, "/nonexistent.sock").Handler()

	cases := []struct {
		name, method, target string
		wantCode             int
		wantSub              string
	}{
		{"healthz", "GET", "/healthz", 200, `"status":"ok"`},
		{"index", "GET", "/", 200, "DCN"},
		{"targets", "GET", "/api/targets", 200, "success"},
		{"query", "GET", "/api/query?query=up", 200, "success"},
		{"containers", "GET", "/api/containers", 502, "docker unreachable"},
		{"method not allowed", "POST", "/api/query", 405, ""},
		// "GET /" is a subtree pattern: it doubles as the SPA catch-all
		{"unknown path serves SPA", "GET", "/nope", 200, "DCN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, c.wantCode, rec.Body)
			}
			if c.wantSub != "" && !strings.Contains(rec.Body.String(), c.wantSub) {
				t.Fatalf("body %s should contain %q", rec.Body, c.wantSub)
			}
		})
	}
}

// proxyProm forwards the fixed Prometheus path with its parameters and passes
// the upstream status code and body through unchanged.
func TestProxyTargets(t *testing.T) {
	t.Run("forwards fixed path", func(t *testing.T) {
		var gotPath, gotState string
		prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotState = r.URL.Query().Get("state")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer prom.Close()

		s := NewServer(prom.URL, "/nonexistent.sock")
		rec := httptest.NewRecorder()
		s.proxyProm("/api/v1/targets?state=active")(rec,
			httptest.NewRequest("GET", "/api/targets", nil))
		if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"data":[]}` {
			t.Fatalf("proxy = %d %s", rec.Code, rec.Body)
		}
		if gotPath != "/api/v1/targets" || gotState != "active" {
			t.Fatalf("upstream saw %s?state=%s", gotPath, gotState)
		}
	})

	t.Run("passes upstream status through", func(t *testing.T) {
		prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusServiceUnavailable)
		}))
		defer prom.Close()

		s := NewServer(prom.URL, "/nonexistent.sock")
		rec := httptest.NewRecorder()
		s.proxyProm("/api/v1/targets?state=active")(rec,
			httptest.NewRequest("GET", "/api/targets", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 passthrough", rec.Code)
		}
	})

	t.Run("upstream unreachable", func(t *testing.T) {
		s := NewServer("http://127.0.0.1:1", "/nonexistent.sock")
		rec := httptest.NewRecorder()
		s.proxyProm("/api/v1/targets?state=active")(rec,
			httptest.NewRequest("GET", "/api/targets", nil))
		if rec.Code != 502 || !strings.Contains(rec.Body.String(), "upstream unreachable") {
			t.Fatalf("proxy = %d %s", rec.Code, rec.Body)
		}
	})
}

// handleQuery escapes the user query before forwarding it to Prometheus.
func TestProxyQueryEscaping(t *testing.T) {
	var got string
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer prom.Close()

	raw := `sum(rate(x[5m])) & up`
	s := NewServer(prom.URL, "/nonexistent.sock")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/query?query="+url.QueryEscape(raw), nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body)
	}
	if got != raw {
		t.Fatalf("upstream query = %q, want %q", got, raw)
	}
}

// handleContainers: an unreachable Docker socket yields a JSON 502.
func TestHandleContainersUnreachable(t *testing.T) {
	s := NewServer("http://prom:9090", filepath.Join(t.TempDir(), "missing.sock"))
	rec := httptest.NewRecorder()
	s.handleContainers(rec, httptest.NewRequest("GET", "/api/containers", nil))
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "docker unreachable") {
		t.Fatalf("resp = %d %s", rec.Code, rec.Body)
	}
}

// handleContainers: lists containers through the Docker Engine API served on a
// fake unix socket and streams the JSON body through.
func TestHandleContainersFromFakeDocker(t *testing.T) {
	dir, err := os.MkdirTemp("", "console-docker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "docker.sock") // short path: unix sockets cap at ~104 chars

	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "1" {
			t.Errorf("docker all param = %q, want 1", r.URL.Query().Get("all"))
		}
		if r.Host != "docker" {
			t.Errorf("docker host = %q, want docker", r.Host)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"Names":["/gns-1"],"State":"running"}]`))
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = http.Serve(ln, mux) }()

	s := NewServer("http://prom:9090", sock)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/containers", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "gns-1") {
		t.Fatalf("containers = %d %s", rec.Code, rec.Body)
	}
}
