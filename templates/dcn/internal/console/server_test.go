package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexServed(t *testing.T) {
	s := NewServer("http://prom:9090", "/nonexistent.sock")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "DCN") {
		t.Fatalf("index not served: code=%d", rec.Code)
	}
}

func TestQueryProxy(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/query") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success"})
	}))
	defer prom.Close()
	s := NewServer(prom.URL, "/nonexistent.sock")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/query?query=up", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "success") {
		t.Fatalf("query proxy failed: %d %s", rec.Code, rec.Body.String())
	}
}
