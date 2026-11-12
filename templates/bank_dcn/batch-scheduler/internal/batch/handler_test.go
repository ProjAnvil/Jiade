package batch

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank-dcn/lib/sqltest"
)

const jobSelect = "SELECT status, total_interest FROM batch_job"

func getJobRequest(bizDate string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/jobs/"+bizDate, nil)
	r.SetPathValue("bizDate", bizDate)
	return r
}

// Handler mounts /healthz, POST /jobs/interest and GET /jobs/{bizDate}.
func TestHandlerRoutes(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		// GET /jobs/{bizDate}: job lookup returns no rows -> 404
		sqltest.Rule{Contains: jobSelect, Columns: []string{"status", "total_interest"}},
	)
	s := NewServer(db, "http://gns-unused")
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("healthz = %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/jobs/interest", strings.NewReader(`{}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "bizDate required") {
		t.Fatalf("create = %d %s, want 400", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jobs/2026-08-02", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "job not found") {
		t.Fatalf("get = %d %s, want 404", rec.Code, rec.Body)
	}
}

// handleGetJob/respondJob render the job with per-unit details; failures are 404/500.
func TestHandleGetJob(t *testing.T) {
	cases := []struct {
		name    string
		rules   []sqltest.Rule
		code    int
		bodyHas []string
	}{
		{"found with units", []sqltest.Rule{
			{Contains: jobSelect, Columns: []string{"status", "total_interest"},
				Rows: [][]any{{"SUCCEEDED", "12.34"}}},
			{Contains: "COALESCE(error,'')",
				Columns: []string{"dcn", "accounts", "interest", "status", "error"},
				Rows: [][]any{
					{"dcn01", int64(10), "1.00", "DONE", ""},
					{"dcn02", int64(5), "2.00", "FAILED", "boom"},
				}},
		}, 200, []string{"SUCCEEDED", "12.34", "dcn01", "dcn02", `"error":"boom"`}},
		{"not found", []sqltest.Rule{
			{Contains: jobSelect, Columns: []string{"status", "total_interest"}},
		}, 404, []string{"job not found"}},
		{"job query error", []sqltest.Rule{
			{Contains: jobSelect, Err: errors.New("job boom")},
		}, 500, []string{"job boom"}},
		{"units query error", []sqltest.Rule{
			{Contains: jobSelect, Columns: []string{"status", "total_interest"},
				Rows: [][]any{{"RUNNING", ""}}},
			{Contains: "COALESCE(error,'')", Err: errors.New("units boom")},
		}, 500, []string{"units boom"}},
		{"unit scan error", []sqltest.Rule{
			{Contains: jobSelect, Columns: []string{"status", "total_interest"},
				Rows: [][]any{{"RUNNING", ""}}},
			{Contains: "COALESCE(error,'')",
				Columns: []string{"dcn", "accounts", "interest", "status", "error"},
				Rows:    [][]any{{"dcn01", "not-a-number", "1.00", "DONE", ""}}},
		}, 500, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := sqltest.NewDB(t, tc.rules...)
			s := NewServer(db, "http://gns-unused")
			w := httptest.NewRecorder()
			s.handleGetJob(w, getJobRequest("2026-08-02"))
			if w.Code != tc.code {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body, tc.code)
			}
			for _, want := range tc.bodyHas {
				if !strings.Contains(w.Body.String(), want) {
					t.Errorf("body %s missing %q", w.Body, want)
				}
			}
		})
	}
}

// finishJob failure is logged only: the response path stays intact.
func TestFinishJobErrorIsLoggedOnly(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "UPDATE batch_job SET status = ?, total_interest",
			Err: errors.New("finish boom")},
	)
	s := NewServer(db, "http://gns-unused")
	s.finishJob("2026-08-02", "SUCCEEDED", "1.00") // must not panic
	if rec.ExecCount("UPDATE batch_job SET status = ?, total_interest") != 1 {
		t.Fatal("the terminal UPDATE must have been attempted")
	}
}
