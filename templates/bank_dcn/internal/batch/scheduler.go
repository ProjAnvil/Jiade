// Package batch implements a standalone batch scheduling service: it triggers
// end-of-day batches (currently interest accrual) by business date, dispatches
// them concurrently across DCN units, aggregates per-unit results, and provides
// idempotent rerun control (simulating a production-grade independent batch
// scheduling platform).
package batch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"bank_dcn/internal/platform/httpx"
	"bank_dcn/internal/platform/metrics"
)

const unitTimeout = 30 * time.Second

// Server is the batch scheduling service.
type Server struct {
	db  *sql.DB
	gns string
	hc  *http.Client
}

// NewServer constructs the batch scheduling service.
func NewServer(db *sql.DB, gns string) *Server {
	return &Server{db: db, gns: gns, hc: &http.Client{Timeout: unitTimeout + 5*time.Second}}
}

// Handler returns the route table; business routes are registered via metrics.Handle to carry RED metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "batch-scheduler", "POST /jobs/interest", http.HandlerFunc(s.handleCreateInterest))
	metrics.Handle(mux, "batch-scheduler", "GET /jobs/{bizDate}", http.HandlerFunc(s.handleGetJob))
	return mux
}

// validateBizDate validates the YYYY-MM-DD format and that it is a real calendar date.
func validateBizDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

type route struct {
	DCN      string `json:"dcn"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// unitResult holds the batch execution result for a single unit.
type unitResult struct {
	DCN      string
	Accounts int
	Interest string
	Err      error
}

func (s *Server) fetchActiveRoutes(ctx context.Context) ([]route, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", s.gns+"/routes", nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var all []route
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, err
	}
	var out []route
	for _, r := range all {
		if r.Status == "ACTIVE" {
			out = append(out, r)
		}
	}
	return out, nil
}

// runOnUnits concurrently calls each unit's interest endpoint and returns results in units order; a single failure records Err without aborting the batch.
func (s *Server) runOnUnits(ctx context.Context, bizDate string, units []route) []unitResult {
	results := make([]unitResult, len(units))
	var wg sync.WaitGroup
	for i, u := range units {
		wg.Add(1)
		go func(i int, u route) {
			defer wg.Done()
			results[i] = s.runOnUnit(ctx, bizDate, u)
		}(i, u)
	}
	wg.Wait()
	return results
}

func (s *Server) runOnUnit(ctx context.Context, bizDate string, u route) unitResult {
	res := unitResult{DCN: u.DCN}
	body, _ := json.Marshal(map[string]string{"bizDate": bizDate})
	ctx, cancel := context.WithTimeout(ctx, unitTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST",
		u.Endpoint+"/internal/batch/interest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		res.Err = fmt.Errorf("unit returned %d: %s", resp.StatusCode, raw)
		return res
	}
	var v struct {
		Accounts      int    `json:"accounts"`
		TotalInterest string `json:"totalInterest"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		res.Err = err
		return res
	}
	res.Accounts, res.Interest = v.Accounts, v.TotalInterest
	return res
}

type jobRequest struct {
	BizDate string `json:"bizDate"`
}

func (s *Server) handleCreateInterest(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := httpx.Decode(r, &req); err != nil || !validateBizDate(req.BizDate) {
		httpx.Error(w, 400, "bizDate required in YYYY-MM-DD")
		return
	}
	var status string
	var stale bool
	err := s.db.QueryRow(
		`SELECT status, created_at < NOW() - INTERVAL 10 MINUTE FROM batch_job WHERE biz_date = ?`,
		req.BizDate).Scan(&status, &stale)
	switch {
	case err == nil && status != "FAILED" && !(status == "RUNNING" && stale):
		s.respondJob(w, req.BizDate) // RUNNING (not stale) / SUCCEEDED: idempotently return current status
		return
	case err == nil:
		// FAILED allows retry: only failed units are rerun (successful units are protected by journal idempotency);
		// stale RUNNING is treated as a zombie (process crash or finishJob failure), also allowing rerun.
		// Reset created_at so the retried job is re-aged, avoiding a false zombie verdict after 10 minutes.
		if _, err := s.db.Exec(
			`UPDATE batch_job SET status = 'RUNNING', finished_at = NULL, created_at = NOW() WHERE biz_date = ?`,
			req.BizDate); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.Exec(
			`INSERT INTO batch_job (biz_date, type, status) VALUES (?,'INTEREST','RUNNING')`,
			req.BizDate); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
	default:
		httpx.Error(w, 500, err.Error())
		return
	}

	units, err := s.fetchActiveRoutes(r.Context())
	if err != nil {
		s.finishJob(req.BizDate, "FAILED", "0")
		httpx.Error(w, 502, "gns unreachable: "+err.Error())
		return
	}
	results := s.runOnUnits(r.Context(), req.BizDate, units)
	failed := false
	sum := decimal.Zero
	for _, res := range results {
		if res.Err == nil {
			f, err := decimal.NewFromString(res.Interest)
			if err != nil {
				// Unit interest parse failure is treated as that unit being FAILED, following the existing failure path
				res.Err = fmt.Errorf("parse unit interest %q: %w", res.Interest, err)
			} else {
				sum = sum.Add(f)
			}
		}
		st, errStr := "DONE", ""
		if res.Err != nil {
			st, errStr = "FAILED", res.Err.Error()
			failed = true
		}
		if _, err := s.db.Exec(
			`INSERT INTO batch_unit_result (biz_date, dcn, accounts, interest, status, error)
			 VALUES (?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE accounts=VALUES(accounts), interest=VALUES(interest),
			   status=VALUES(status), error=VALUES(error)`,
			req.BizDate, res.DCN, res.Accounts, res.Interest, st, errStr); err != nil {
			// On result persistence failure, mark the job FAILED before returning: FAILED allows retry (retry overwrites unit results), preventing the job from being stuck in RUNNING forever.
			s.finishJob(req.BizDate, "FAILED", "0")
			httpx.Error(w, 500, err.Error())
			return
		}
	}
	total := sum.StringFixedBank(2)
	if failed {
		s.finishJob(req.BizDate, "FAILED", total)
	} else {
		s.finishJob(req.BizDate, "SUCCEEDED", total)
	}
	s.respondJob(w, req.BizDate)
}

func (s *Server) finishJob(bizDate, status, total string) {
	if _, err := s.db.Exec(
		`UPDATE batch_job SET status = ?, total_interest = ?, finished_at = NOW() WHERE biz_date = ?`,
		status, total, bizDate); err != nil {
		// Terminal-state persistence failure is not returned as an error (the caller has no response path left), but must be visible:
		// a missed write leaves a zombie RUNNING that can only be recovered by the 10-minute stale-retry fallback.
		log.Printf("finishJob %s -> %s: %v", bizDate, status, err)
	}
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	s.respondJob(w, r.PathValue("bizDate"))
}

// respondJob renders the job view (status + per-unit details).
func (s *Server) respondJob(w http.ResponseWriter, bizDate string) {
	var status, total string
	err := s.db.QueryRow(
		`SELECT status, total_interest FROM batch_job WHERE biz_date = ?`, bizDate).
		Scan(&status, &total)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, 404, "job not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	rows, err := s.db.Query(
		`SELECT dcn, accounts, interest, status, COALESCE(error,'') FROM batch_unit_result
		 WHERE biz_date = ? ORDER BY dcn`, bizDate)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type unitView struct {
		DCN      string `json:"dcn"`
		Accounts int    `json:"accounts"`
		Interest string `json:"interest"`
		Status   string `json:"status"`
		Error    string `json:"error,omitempty"`
	}
	units := []unitView{}
	for rows.Next() {
		var u unitView
		if err := rows.Scan(&u.DCN, &u.Accounts, &u.Interest, &u.Status, &u.Error); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		units = append(units, u)
	}
	if err := rows.Err(); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, map[string]any{
		"bizDate": bizDate, "type": "INTEREST", "status": status,
		"totalInterest": total, "units": units,
	})
}
