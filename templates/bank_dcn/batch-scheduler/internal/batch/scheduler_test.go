package batch

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestValidateBizDate(t *testing.T) {
	for _, ok := range []string{"2026-08-02", "2026-12-31"} {
		if !validateBizDate(ok) {
			t.Errorf("validateBizDate(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "2026-8-2", "2026-13-01", "2026-02-30", "today"} {
		if validateBizDate(bad) {
			t.Errorf("validateBizDate(%q) = true, want false", bad)
		}
	}
}

// runOnUnits concurrently calls each unit's interest endpoint and aggregates results (a single unit failure does not abort the batch).
func TestRunOnUnits(t *testing.T) {
	okUnit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dcn": "dcn01", "accounts": 50, "totalInterest": "5.00",
		})
	}))
	defer okUnit.Close()
	badUnit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer badUnit.Close()

	s := &Server{hc: okUnit.Client()}
	units := []route{
		{DCN: "dcn01", Endpoint: okUnit.URL, Status: "ACTIVE"},
		{DCN: "dcn02", Endpoint: badUnit.URL, Status: "ACTIVE"},
		{DCN: "dcn03", Endpoint: "http://127.0.0.1:1", Status: "ACTIVE"}, // connection failure
	}
	results := s.runOnUnits(context.Background(), "2026-08-02", units)
	if len(results) != 3 {
		t.Fatalf("results len = %d", len(results))
	}
	if results[0].Err != nil || results[0].Accounts != 50 || results[0].Interest != "5.00" {
		t.Errorf("dcn01 result = %+v", results[0])
	}
	if results[1].Err == nil {
		t.Errorf("dcn02 should fail with HTTP 500")
	}
	if results[2].Err == nil {
		t.Errorf("dcn03 should fail with connection error")
	}
}

// ---- In-memory database/sql/driver (no external deps like sqlmock), used to drive handler-level tests ----

var errFake = errors.New("fake driver: unsupported")

// fakeDriver behavior contract: SELECT always returns an empty result set; writes to batch_unit_result always error
// (simulating upsert failure); all other writes succeed and are recorded.
type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errFake }
func (c *fakeConn) Close() error                        { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)           { return nil, errFake }

func (c *fakeConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	if strings.Contains(query, "batch_unit_result") {
		return nil, errors.New("unit result upsert boom")
	}
	recordJobUpdate(query, args)
	return fakeResult{}, nil
}

func (c *fakeConn) Query(string, []driver.Value) (driver.Rows, error) { return &fakeRows{}, nil }

type fakeRows struct{}

func (r *fakeRows) Columns() []string         { return []string{"status"} }
func (r *fakeRows) Close() error              { return nil }
func (r *fakeRows) Next([]driver.Value) error { return io.EOF }

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

var (
	registerFakeDriver sync.Once
	jobUpdatesMu       sync.Mutex
	jobUpdates         [][]driver.Value // arguments of each batch_job write operation
)

func recordJobUpdate(query string, args []driver.Value) {
	if !strings.Contains(query, "batch_job") {
		return
	}
	jobUpdatesMu.Lock()
	defer jobUpdatesMu.Unlock()
	jobUpdates = append(jobUpdates, append([]driver.Value{query}, args...))
}

func openFakeDB(t *testing.T) *sql.DB {
	t.Helper()
	registerFakeDriver.Do(func() { sql.Register("batch-fake", fakeDriver{}) })
	db, err := sql.Open("batch-fake", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	jobUpdatesMu.Lock()
	jobUpdates = nil
	jobUpdatesMu.Unlock()
	return db
}

// handleCreateInterest should mark the job FAILED before returning 500 when a per-unit result upsert fails,
// so the biz_date can be retried later (the FAILED branch allows rerun), rather than being stuck in RUNNING forever.
func TestHandleCreateInterestUpsertFailureMarksJobFailed(t *testing.T) {
	unit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"accounts": 10, "totalInterest": "1.00"})
	}))
	defer unit.Close()
	gns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]route{{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"}})
	}))
	defer gns.Close()

	s := NewServer(openFakeDB(t), gns.URL)
	req := httptest.NewRequest("POST", "/jobs/interest",
		strings.NewReader(`{"bizDate":"2026-08-02"}`))
	rec := httptest.NewRecorder()
	s.handleCreateInterest(rec, req)

	if rec.Code != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", rec.Code, rec.Body)
	}
	jobUpdatesMu.Lock()
	defer jobUpdatesMu.Unlock()
	var finish []driver.Value
	for _, u := range jobUpdates {
		if strings.Contains(u[0].(string), "UPDATE batch_job SET status = ?, total_interest") {
			finish = u
		}
	}
	if finish == nil {
		t.Fatalf("finishJob was not called; job would be stuck RUNNING. updates: %v", jobUpdates)
	}
	if finish[1] != "FAILED" || finish[2] != "0" || finish[3] != "2026-08-02" {
		t.Errorf("finishJob args = %v, want status=FAILED total=0 bizDate=2026-08-02", finish[1:])
	}
}
