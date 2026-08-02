// Package batch 实现独立批量调度服务：按业务日期发起日终批量（本期为结息），
// 并发调度各 DCN 单元、归集分单元结果、幂等重跑控制（仿真生产独立批量调度平台）。
package batch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"dcn/internal/platform/httpx"
	"dcn/internal/platform/metrics"
)

const unitTimeout = 30 * time.Second

// Server 是批量调度服务。
type Server struct {
	db  *sql.DB
	gns string
	hc  *http.Client
}

// NewServer 构造调度服务。
func NewServer(db *sql.DB, gns string) *Server {
	return &Server{db: db, gns: gns, hc: &http.Client{Timeout: unitTimeout + 5*time.Second}}
}

// Handler 返回路由；业务路由经 metrics.Handle 注册以带 RED 指标。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "batch-scheduler", "POST /jobs/interest", http.HandlerFunc(s.handleCreateInterest))
	metrics.Handle(mux, "batch-scheduler", "GET /jobs/{bizDate}", http.HandlerFunc(s.handleGetJob))
	return mux
}

// validateBizDate 校验 YYYY-MM-DD 且为真实日期。
func validateBizDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

type route struct {
	DCN      string `json:"dcn"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// unitResult 是一个单元的批量执行结果。
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

// runOnUnits 并发调各单元结息端点，按 units 顺序返回结果；单点失败记 Err 不中断。
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
	err := s.db.QueryRow(`SELECT status FROM batch_job WHERE biz_date = ?`, req.BizDate).Scan(&status)
	switch {
	case err == nil && status != "FAILED":
		s.respondJob(w, req.BizDate) // RUNNING/SUCCEEDED：幂等返回当前状态
		return
	case err == nil: // FAILED：允许重试，仅重跑失败单元（成功单元靠 journal 幂等兜底）
		if _, err := s.db.Exec(
			`UPDATE batch_job SET status = 'RUNNING', finished_at = NULL WHERE biz_date = ?`,
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
	total := "0"
	failed := false
	var sum float64
	for _, res := range results {
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
			httpx.Error(w, 500, err.Error())
			return
		}
		if res.Err == nil {
			var f float64
			fmt.Sscanf(res.Interest, "%f", &f)
			sum += f
		}
	}
	total = fmt.Sprintf("%.2f", sum)
	if failed {
		s.finishJob(req.BizDate, "FAILED", total)
	} else {
		s.finishJob(req.BizDate, "SUCCEEDED", total)
	}
	s.respondJob(w, req.BizDate)
}

func (s *Server) finishJob(bizDate, status, total string) {
	_, _ = s.db.Exec(
		`UPDATE batch_job SET status = ?, total_interest = ?, finished_at = NOW() WHERE biz_date = ?`,
		status, total, bizDate)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	s.respondJob(w, r.PathValue("bizDate"))
}

// respondJob 输出任务视图（状态 + 分单元明细）。
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
	httpx.JSON(w, 200, map[string]any{
		"bizDate": bizDate, "type": "INTEREST", "status": status,
		"totalInterest": total, "units": units,
	})
}
