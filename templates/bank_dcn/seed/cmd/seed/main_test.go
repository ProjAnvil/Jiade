package main

import (
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPersonNameDeterministic(t *testing.T) {
	a := personName(rand.New(rand.NewSource(42)))
	b := personName(rand.New(rand.NewSource(42)))
	if a != b || a == "" {
		t.Fatalf("name not deterministic or empty: %q vs %q", a, b)
	}
}

var twoDecimals = regexp.MustCompile(`^\d+\.\d{2}$`)

func TestInitialBalance(t *testing.T) {
	// The first 2 accounts of each unit are fixed at 1000.00 (depended on by verify and the README examples)
	for _, i := range []int{0, 1} {
		if got := initialBalance(rand.New(rand.NewSource(1)), i); got != "1000.00" {
			t.Fatalf("fixed account %d balance = %s, want 1000.00", i, got)
		}
	}
	// The rest are deterministic-random, within [100.00, 100000.00], with 2 decimal places
	r1, r2 := rand.New(rand.NewSource(7)), rand.New(rand.NewSource(7))
	for i := 2; i < 50; i++ {
		b1, b2 := initialBalance(r1, i), initialBalance(r2, i)
		if b1 != b2 {
			t.Fatalf("non-deterministic balance at %d: %s vs %s", i, b1, b2)
		}
		if !twoDecimals.MatchString(b1) {
			t.Fatalf("balance at %d not 2-decimal: %q", i, b1)
		}
		v, err := strconv.ParseFloat(b1, 64)
		if err != nil {
			t.Fatalf("balance at %d not parseable: %q: %v", i, b1, err)
		}
		if v < 100.00 || v > 100000.00 {
			t.Fatalf("balance at %d out of [100.00, 100000.00]: %s", i, b1)
		}
	}
}

// gnsRequest is what the fake GNS saw for one call.
type gnsRequest struct {
	path        string
	contentType string
	body        string
}

// newFakeGNS starts a GNS stub that records every request and replies with the
// given status and body. The server is closed on test cleanup.
func newFakeGNS(t *testing.T, status int, responseBody string) (*httptest.Server, func() []gnsRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []gnsRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, gnsRequest{
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			body:        string(raw),
		})
		mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(ts.Close)
	return ts, func() []gnsRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]gnsRequest(nil), got...)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// slowClient bounds runaway dials so a connection-refused test can never hang.
func slowClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func TestEnvOr(t *testing.T) {
	const key = "SEED_TEST_ENV_OR"
	t.Setenv(key, "custom")
	if got := envOr(key, "def"); got != "custom" {
		t.Fatalf("envOr(%s) = %q, want custom", key, got)
	}
	t.Setenv(key, "") // empty value counts as unset
	if got := envOr(key, "def"); got != "def" {
		t.Fatalf("envOr(empty %s) = %q, want def", key, got)
	}
	if got := envOr("SEED_TEST_ENV_OR_NEVER_SET", "def"); got != "def" {
		t.Fatalf("envOr(unset) = %q, want def", got)
	}
}

func TestPerScaleCount(t *testing.T) {
	// The counts map drives the loop; "full" is asserted here so the test suite
	// never needs to make 2000x3 HTTP calls.
	tests := []struct {
		scale  string
		want   int
		wantOK bool
	}{
		{"dev", 50, true},
		{"full", 2000, true},
		{"bogus", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := perScaleCount(tt.scale)
		if got != tt.want || ok != tt.wantOK {
			t.Fatalf("perScaleCount(%q) = %d, %v; want %d, %v", tt.scale, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestOpenAccountsDevScale(t *testing.T) {
	ts, requests := newFakeGNS(t, http.StatusOK, `{"accountNo":"A"}`)

	var openErr error
	out := captureStdout(t, func() {
		openErr = openAccounts(ts.URL, slowClient(), "dev")
	})
	if openErr != nil {
		t.Fatalf("openAccounts: %v", openErr)
	}

	reqs := requests()
	if len(reqs) != 150 { // 50 per segment x segments 1000/2000/3000
		t.Fatalf("got %d requests, want 150", len(reqs))
	}

	seen := map[string]bool{}
	for _, req := range reqs {
		if req.path != "/accounts" {
			t.Fatalf("path = %q, want /accounts", req.path)
		}
		if req.contentType != "application/json" {
			t.Fatalf("content-type = %q, want application/json", req.contentType)
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(req.body), &payload); err != nil {
			t.Fatalf("body not a string map: %q: %v", req.body, err)
		}
		if payload["name"] == "" {
			t.Fatalf("empty name in body: %q", req.body)
		}
		if payload["requestId"] == "" || seen[payload["requestId"]] {
			t.Fatalf("missing or duplicate requestId: %q", payload["requestId"])
		}
		seen[payload["requestId"]] = true
		if !strings.HasPrefix(payload["requestId"], "seed-dev-") {
			t.Fatalf("requestId %q lacks seed-dev- prefix", payload["requestId"])
		}
		bal, ok := payload["initBalance"]
		if !ok || !twoDecimals.MatchString(bal) {
			t.Fatalf("bad initBalance in body: %q", req.body)
		}
	}
	// The exact requestId set: seed-dev-<seg>-<i> for the 3 segments x 50 accounts.
	if len(seen) != 150 {
		t.Fatalf("unique requestIds = %d, want 150", len(seen))
	}
	for _, seg := range []int{1000, 2000, 3000} {
		for i := 0; i < 50; i++ {
			id := "seed-dev-" + strconv.Itoa(seg) + "-" + strconv.Itoa(i)
			if !seen[id] {
				t.Fatalf("missing expected requestId %q", id)
			}
		}
	}

	// stdout: one "seeded: <gns body>" line per account, and no "seed done" (main prints that).
	if n := strings.Count(out, "seeded: {\"accountNo\":\"A\"}\n"); n != 150 {
		t.Fatalf("seeded lines = %d, want 150", n)
	}
	if strings.Contains(out, "seed done") {
		t.Fatalf("openAccounts must not print %q: %q", "seed done", out)
	}
}

func TestOpenAccountsFixedFirstTwoPerSegment(t *testing.T) {
	ts, requests := newFakeGNS(t, http.StatusOK, "")
	if err := openAccounts(ts.URL, slowClient(), "dev"); err != nil {
		t.Fatalf("openAccounts: %v", err)
	}
	for _, req := range requests() {
		var payload map[string]string
		if err := json.Unmarshal([]byte(req.body), &payload); err != nil {
			t.Fatalf("body not a string map: %q: %v", req.body, err)
		}
		id := payload["requestId"]
		fixed := strings.HasSuffix(id, "-0") || strings.HasSuffix(id, "-1")
		if fixed && payload["initBalance"] != "1000.00" {
			t.Fatalf("first-2 account %s has balance %q, want 1000.00", id, payload["initBalance"])
		}
	}
}

func TestOpenAccountsDeterministic(t *testing.T) {
	// Same scale must replay the exact same request sequence (name, balance, requestId).
	ts, requests := newFakeGNS(t, http.StatusOK, "")
	run := func() {
		captureStdout(t, func() {
			if err := openAccounts(ts.URL, slowClient(), "dev"); err != nil {
				t.Fatalf("openAccounts: %v", err)
			}
		})
	}
	run()
	first := requests()
	run()
	second := requests()[len(first):] // the recorder accumulates; keep only run 2
	if len(first) != 150 || len(second) != 150 {
		t.Fatalf("run lengths = %d, %d; want 150, 150", len(first), len(second))
	}
	for i := range first {
		if first[i].body != second[i].body {
			t.Fatalf("request %d differs between runs: %q vs %q", i, first[i].body, second[i].body)
		}
	}
}

func TestOpenAccountsGNSFailure(t *testing.T) {
	ts, _ := newFakeGNS(t, http.StatusInternalServerError, "boom")
	err := openAccounts(ts.URL, slowClient(), "dev")
	if err == nil {
		t.Fatal("want error on GNS 500, got nil")
	}
	if !strings.Contains(err.Error(), "GNS returned 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error should surface status and body, got: %v", err)
	}
}

func TestOpenAccountsConnectionRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close() // release the port so dialing fails
	err := openAccounts(url, slowClient(), "dev")
	if err == nil {
		t.Fatal("want error on connection refused, got nil")
	}
	if !strings.Contains(err.Error(), "open account via GNS") {
		t.Fatalf("error should carry GNS context, got: %v", err)
	}
}

func TestOpenAccountsUnknownScale(t *testing.T) {
	ts, requests := newFakeGNS(t, http.StatusOK, "")
	err := openAccounts(ts.URL, slowClient(), "bogus")
	if err == nil || !strings.Contains(err.Error(), `unknown scale "bogus"`) {
		t.Fatalf("want unknown-scale error, got %v", err)
	}
	if n := len(requests()); n != 0 {
		t.Fatalf("unknown scale must not hit GNS, got %d requests", n)
	}
}
