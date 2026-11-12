package batch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"bank-dcn/lib/sqltest"
)

// gnsRoutes serves a /routes listing for fetchActiveRoutes.
func gnsRoutes(routes ...route) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /routes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(routes)
	})
	return httptest.NewServer(mux)
}

// unitInterest serves the /internal/batch/interest endpoint of one DCN unit, counting calls.
func unitInterest(t *testing.T, accounts int, interest string, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		if r.URL.Path != "/internal/batch/interest" {
			t.Errorf("unit called on %s, want /internal/batch/interest", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"accounts":` + strconv.Itoa(accounts) + `,"totalInterest":"` + interest + `"}`))
	}))
}

func createInterestRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/jobs/interest", strings.NewReader(body))
}

// respondRules script the two reads of the job view rendered after a run; the unit-rows
// read always happens (respondJob renders an empty list when no rows are returned).
func respondRules(status, total string, units bool) []sqltest.Rule {
	unitRows := [][]any{}
	if units {
		unitRows = [][]any{{"dcn01", int64(10), "1.00", "DONE", ""}}
	}
	return []sqltest.Rule{
		{Contains: "SELECT status, total_interest FROM batch_job",
			Columns: []string{"status", "total_interest"}, Rows: [][]any{{status, total}}},
		{Contains: "COALESCE(error,'')",
			Columns: []string{"dcn", "accounts", "interest", "status", "error"},
			Rows:    unitRows},
	}
}

// fetchActiveRoutes: only ACTIVE routes are returned; connection and payload failures error.
func TestFetchActiveRoutes(t *testing.T) {
	t.Run("filters to ACTIVE", func(t *testing.T) {
		gns := gnsRoutes(
			route{DCN: "dcn01", Endpoint: "http://u1", Status: "ACTIVE"},
			route{DCN: "dcn02", Endpoint: "http://u2", Status: "INACTIVE"},
			route{DCN: "dcn03", Endpoint: "http://u3", Status: "ACTIVE"},
		)
		defer gns.Close()
		s := &Server{gns: gns.URL, hc: gns.Client()}
		routes, err := s.fetchActiveRoutes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(routes) != 2 || routes[0].DCN != "dcn01" || routes[1].DCN != "dcn03" {
			t.Fatalf("routes = %+v, want dcn01+dcn03", routes)
		}
	})
	t.Run("connection error", func(t *testing.T) {
		gns := httptest.NewServer(http.NotFoundHandler())
		gns.Close()
		s := &Server{gns: gns.URL, hc: &http.Client{}}
		if _, err := s.fetchActiveRoutes(context.Background()); err == nil {
			t.Fatal("unreachable gns must error")
		}
	})
	t.Run("bad payload", func(t *testing.T) {
		gns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer gns.Close()
		s := &Server{gns: gns.URL, hc: gns.Client()}
		if _, err := s.fetchActiveRoutes(context.Background()); err == nil {
			t.Fatal("undecodable routes must error")
		}
	})
}

// handleCreateInterest rejects malformed bodies and invalid business dates.
func TestHandleCreateInterestValidation(t *testing.T) {
	db, _ := sqltest.NewDB(t)
	s := NewServer(db, "http://gns-unused")
	for _, body := range []string{"not json", `{}`, `{"bizDate":"2026-13-01"}`, `{"bizDate":"2026-02-30"}`} {
		w := httptest.NewRecorder()
		s.handleCreateInterest(w, createInterestRequest(body))
		if w.Code != 400 {
			t.Fatalf("body %s = %d (%s), want 400", body, w.Code, w.Body)
		}
	}
}

// A brand-new bizDate inserts a RUNNING job, runs the ACTIVE units and finishes SUCCEEDED
// with the summed interest; INACTIVE routes are never dialed.
func TestHandleCreateInterestNewJobSucceeds(t *testing.T) {
	var calls int32
	unit := unitInterest(t, 10, "1.00", &calls)
	defer unit.Close()
	gns := gnsRoutes(
		route{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"},
		route{DCN: "dcn09", Endpoint: "http://never-dialed", Status: "INACTIVE"},
	)
	defer gns.Close()

	db, rec := sqltest.NewDB(t, append([]sqltest.Rule{
		{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
			Columns: []string{"status", "stale"}}, // no rows -> new job
		{Contains: "INSERT INTO batch_job"},
		{Contains: "INSERT INTO batch_unit_result"},
		{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	}, respondRules("SUCCEEDED", "1.00", true)...)...)

	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "SUCCEEDED") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("unit calls = %d, want 1 (INACTIVE must be skipped)", calls)
	}
	if rec.ExecCount("INSERT INTO batch_job") != 1 {
		t.Fatal("a new job row must be inserted")
	}
	finish := rec.LastExec("UPDATE batch_job SET status = ?, total_interest")
	if finish == nil || finish.Args[0] != "SUCCEEDED" || finish.Args[1] != "1.00" ||
		finish.Args[2] != "2026-08-02" {
		t.Fatalf("finishJob args = %v, want SUCCEEDED/1.00/2026-08-02", finish.Args)
	}
}

// SUCCEEDED and fresh RUNNING jobs are returned idempotently without rerunning units.
func TestHandleCreateInterestIdempotent(t *testing.T) {
	for _, st := range []string{"SUCCEEDED", "RUNNING"} {
		t.Run(st, func(t *testing.T) {
			var calls int32
			unit := unitInterest(t, 10, "1.00", &calls)
			defer unit.Close()
			gns := gnsRoutes(route{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"})
			defer gns.Close()

			db, rec := sqltest.NewDB(t, append([]sqltest.Rule{
				{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
					Columns: []string{"status", "stale"}, Rows: [][]any{{st, false}}},
			}, respondRules(st, "1.00", true)...)...)

			s := NewServer(db, gns.URL)
			w := httptest.NewRecorder()
			s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
			if w.Code != 200 {
				t.Fatalf("status = %d body = %s", w.Code, w.Body)
			}
			if atomic.LoadInt32(&calls) != 0 {
				t.Fatal("an idempotent hit must not rerun units")
			}
			if rec.ExecCount("INSERT INTO batch_job") != 0 ||
				rec.ExecCount("INSERT INTO batch_unit_result") != 0 {
				t.Fatal("an idempotent hit must not write job or unit rows")
			}
		})
	}
}

// A FAILED job allows retry: the job is reset to RUNNING and the units run again.
func TestHandleCreateInterestRerunsFailedJob(t *testing.T) {
	var calls int32
	unit := unitInterest(t, 10, "1.00", &calls)
	defer unit.Close()
	gns := gnsRoutes(route{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"})
	defer gns.Close()

	db, rec := sqltest.NewDB(t, append([]sqltest.Rule{
		{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
			Columns: []string{"status", "stale"}, Rows: [][]any{{"FAILED", true}}},
		{Contains: "UPDATE batch_job SET status = 'RUNNING'"},
		{Contains: "INSERT INTO batch_unit_result"},
		{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	}, respondRules("SUCCEEDED", "1.00", true)...)...)

	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if rec.ExecCount("UPDATE batch_job SET status = 'RUNNING'") != 1 {
		t.Fatal("a FAILED job must be reset to RUNNING before rerun")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatal("units must run on retry")
	}
}

// A stale RUNNING job (zombie) is treated like FAILED: reset and retried; if GNS is
// unreachable the job is marked FAILED so the bizDate stays retryable.
func TestHandleCreateInterestStaleRunningWithGnsDown(t *testing.T) {
	gns := httptest.NewServer(http.NotFoundHandler())
	gns.Close()

	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
			Columns: []string{"status", "stale"}, Rows: [][]any{{"RUNNING", true}}},
		sqltest.Rule{Contains: "UPDATE batch_job SET status = 'RUNNING'"},
		sqltest.Rule{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	)
	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "gns unreachable") {
		t.Fatalf("status = %d body = %s, want 502", w.Code, w.Body)
	}
	if rec.ExecCount("UPDATE batch_job SET status = 'RUNNING'") != 1 {
		t.Fatal("a stale RUNNING job must be reset")
	}
	finish := rec.LastExec("UPDATE batch_job SET status = ?, total_interest")
	if finish == nil || finish.Args[0] != "FAILED" || finish.Args[1] != "0" {
		t.Fatalf("finishJob args = %v, want FAILED/0", finish.Args)
	}
}

// A GNS outage on a brand-new job also finishes FAILED before returning 502.
func TestHandleCreateInterestGnsUnreachableMarksFailed(t *testing.T) {
	gns := httptest.NewServer(http.NotFoundHandler())
	gns.Close()

	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
			Columns: []string{"status", "stale"}}, // no rows
		sqltest.Rule{Contains: "INSERT INTO batch_job"},
		sqltest.Rule{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	)
	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 502 {
		t.Fatalf("status = %d body = %s, want 502", w.Code, w.Body)
	}
	finish := rec.LastExec("UPDATE batch_job SET status = ?, total_interest")
	if finish == nil || finish.Args[0] != "FAILED" {
		t.Fatalf("finishJob args = %v, want FAILED", finish.Args)
	}
}

// No ACTIVE routes: the batch trivially succeeds with a zero total.
func TestHandleCreateInterestNoActiveRoutes(t *testing.T) {
	var calls int32
	unit := unitInterest(t, 10, "1.00", &calls)
	defer unit.Close()
	gns := gnsRoutes(route{DCN: "dcn01", Endpoint: unit.URL, Status: "INACTIVE"})
	defer gns.Close()

	db, rec := sqltest.NewDB(t, append([]sqltest.Rule{
		{Contains: "created_at < NOW() - INTERVAL 10 MINUTE", Columns: []string{"status", "stale"}},
		{Contains: "INSERT INTO batch_job"},
		{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	}, respondRules("SUCCEEDED", "0.00", false)...)...)

	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("no unit may be dialed without ACTIVE routes")
	}
	finish := rec.LastExec("UPDATE batch_job SET status = ?, total_interest")
	if finish == nil || finish.Args[0] != "SUCCEEDED" || finish.Args[1] != "0.00" {
		t.Fatalf("finishJob args = %v, want SUCCEEDED/0.00", finish.Args)
	}
}

// A unit reporting a non-decimal interest is treated as FAILED; the job finishes FAILED.
func TestHandleCreateInterestUnitParseFailureMarksFailed(t *testing.T) {
	var calls int32
	unit := unitInterest(t, 1, "abc", &calls) // totalInterest not a decimal
	defer unit.Close()
	gns := gnsRoutes(route{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"})
	defer gns.Close()

	db, rec := sqltest.NewDB(t, append([]sqltest.Rule{
		{Contains: "created_at < NOW() - INTERVAL 10 MINUTE", Columns: []string{"status", "stale"}},
		{Contains: "INSERT INTO batch_job"},
		{Contains: "INSERT INTO batch_unit_result"},
		{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	}, respondRules("FAILED", "0.00", false)...)...)

	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	upsert := rec.LastExec("INSERT INTO batch_unit_result")
	if upsert == nil || upsert.Args[4] != "FAILED" {
		t.Fatalf("unit result args = %v, want status FAILED", upsert.Args)
	}
	finish := rec.LastExec("UPDATE batch_job SET status = ?, total_interest")
	if finish == nil || finish.Args[0] != "FAILED" {
		t.Fatalf("finishJob args = %v, want FAILED", finish.Args)
	}
}

// A finishJob write failure is tolerated: the response still renders the job view.
func TestHandleCreateInterestFinishJobFailureTolerated(t *testing.T) {
	var calls int32
	unit := unitInterest(t, 10, "1.00", &calls)
	defer unit.Close()
	gns := gnsRoutes(route{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"})
	defer gns.Close()

	db, _ := sqltest.NewDB(t, append([]sqltest.Rule{
		{Contains: "created_at < NOW() - INTERVAL 10 MINUTE", Columns: []string{"status", "stale"}},
		{Contains: "INSERT INTO batch_job"},
		{Contains: "INSERT INTO batch_unit_result"},
		{Contains: "UPDATE batch_job SET status = ?, total_interest", Err: errors.New("finish boom")},
	}, respondRules("RUNNING", "", false)...)...)

	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s, want 200 despite finishJob failure", w.Code, w.Body)
	}
}

// Job-row write failures surface as 500.
func TestHandleCreateInterestWriteFailures(t *testing.T) {
	t.Run("job query error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "created_at < NOW() - INTERVAL 10 MINUTE", Err: errors.New("job boom")},
		)
		s := NewServer(db, "http://gns-unused")
		w := httptest.NewRecorder()
		s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "job boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("insert error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
				Columns: []string{"status", "stale"}},
			sqltest.Rule{Contains: "INSERT INTO batch_job", Err: errors.New("insert boom")},
		)
		s := NewServer(db, "http://gns-unused")
		w := httptest.NewRecorder()
		s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "insert boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
	t.Run("reset error", func(t *testing.T) {
		db, _ := sqltest.NewDB(t,
			sqltest.Rule{Contains: "created_at < NOW() - INTERVAL 10 MINUTE",
				Columns: []string{"status", "stale"}, Rows: [][]any{{"FAILED", true}}},
			sqltest.Rule{Contains: "UPDATE batch_job SET status = 'RUNNING'", Err: errors.New("reset boom")},
		)
		s := NewServer(db, "http://gns-unused")
		w := httptest.NewRecorder()
		s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
		if w.Code != 500 || !strings.Contains(w.Body.String(), "reset boom") {
			t.Fatalf("status = %d body = %s", w.Code, w.Body)
		}
	})
}

// A unit answering 200 with a non-JSON body fails that unit only; the job finishes FAILED.
func TestHandleCreateInterestUnitGarbageBodyMarksFailed(t *testing.T) {
	unit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer unit.Close()
	gns := gnsRoutes(route{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"})
	defer gns.Close()

	db, rec := sqltest.NewDB(t, append([]sqltest.Rule{
		{Contains: "created_at < NOW() - INTERVAL 10 MINUTE", Columns: []string{"status", "stale"}},
		{Contains: "INSERT INTO batch_job"},
		{Contains: "INSERT INTO batch_unit_result"},
		{Contains: "UPDATE batch_job SET status = ?, total_interest"},
	}, respondRules("FAILED", "0.00", false)...)...)

	s := NewServer(db, gns.URL)
	w := httptest.NewRecorder()
	s.handleCreateInterest(w, createInterestRequest(`{"bizDate":"2026-08-02"}`))
	if w.Code != 200 {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	finish := rec.LastExec("UPDATE batch_job SET status = ?, total_interest")
	if finish == nil || finish.Args[0] != "FAILED" {
		t.Fatalf("finishJob args = %v, want FAILED", finish.Args)
	}
}
