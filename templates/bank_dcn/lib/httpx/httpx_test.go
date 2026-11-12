package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, map[string]int{"code": 42})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	// json.Encoder appends a trailing newline.
	if got, want := rec.Body.String(), "{\"code\":42}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestError(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, http.StatusBadRequest, "bad input")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	if got, want := rec.Body.String(), "{\"error\":\"bad input\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestDecode(t *testing.T) {
	t.Run("valid body", func(t *testing.T) {
		var v struct {
			Name string `json:"name"`
		}
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"alice"}`))
		if err := Decode(req, &v); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if v.Name != "alice" {
			t.Fatalf("name = %q, want alice", v.Name)
		}
	})

	t.Run("malformed body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{not json`))
		var v map[string]any
		if err := Decode(req, &v); err == nil {
			t.Fatal("Decode should fail on malformed JSON")
		}
	})
}
