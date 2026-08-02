// Package gns implements the GNS global routing service: customer → DCN mapping, account opening allocation, and segment management.
package gns

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"bank_dcn/internal/platform/httpx"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mysqlx"
)

const cacheTTL = time.Hour

// Server is the GNS service.
type Server struct {
	db    *sql.DB
	cache *redis.Client
	hc    *http.Client
}

// NewServer constructs the GNS service.
func NewServer(db *sql.DB, cache *redis.Client) *Server {
	return &Server{db: db, cache: cache, hc: &http.Client{Timeout: 5 * time.Second}}
}

// Handler returns the route table; business routes are registered via metrics.Handle to carry RED metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "gns", "GET /locate", http.HandlerFunc(s.handleLocate))
	metrics.Handle(mux, "gns", "POST /accounts", http.HandlerFunc(s.handleOpenAccount))
	metrics.Handle(mux, "gns", "GET /routes", http.HandlerFunc(s.handleListRoutes))
	metrics.Handle(mux, "gns", "POST /routes", http.HandlerFunc(s.handleAddRoute))
	return mux
}

// LocateResult is the response body of /locate and /accounts.
type LocateResult struct {
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Endpoint  string `json:"endpoint"`
}

// ErrNotFound indicates the account has no routing record.
var ErrNotFound = errors.New("account not routed")

func cacheKey(id int) string { return fmt.Sprintf("route:%d", id) }

// Locate checks the Redis cache first; on miss it falls back to MySQL and backfills the cache.
func (s *Server) Locate(ctx context.Context, id int) (*LocateResult, error) {
	if v, err := s.cache.Get(ctx, cacheKey(id)).Result(); err == nil {
		if parts := strings.SplitN(v, "|", 2); len(parts) == 2 {
			return &LocateResult{AccountID: id, DCN: parts[0], Endpoint: parts[1]}, nil
		}
	}
	res := &LocateResult{AccountID: id}
	err := s.db.QueryRowContext(ctx,
		`SELECT ar.dcn, rs.endpoint FROM account_route ar
		 JOIN route_segment rs ON rs.dcn = ar.dcn WHERE ar.account_id = ?`, id).
		Scan(&res.DCN, &res.Endpoint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.cache.Set(ctx, cacheKey(id), res.DCN+"|"+res.Endpoint, cacheTTL)
	return res, nil
}

func (s *Server) handleLocate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("accountId"))
	if err != nil {
		httpx.Error(w, 400, "accountId must be an integer")
		return
	}
	res, err := s.Locate(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpx.Error(w, 404, "account not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, res)
}

type openAccountRequest struct {
	Name        string `json:"name"`
	InitBalance string `json:"initBalance"`
	RequestID   string `json:"requestId,omitempty"`
}

func (s *Server) handleOpenAccount(w http.ResponseWriter, r *http.Request) {
	var req openAccountRequest
	if err := httpx.Decode(r, &req); err != nil || req.Name == "" || req.InitBalance == "" {
		httpx.Error(w, 400, "name and initBalance required")
		return
	}
	// Idempotency: a requestId hit returns the original account opening result directly
	if req.RequestID != "" {
		if res, err := s.findByRequestID(r.Context(), req.RequestID); err == nil {
			httpx.JSON(w, 200, res)
			return
		}
	}
	segs, err := s.listSegments(r.Context())
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	counts, err := s.accountCounts(r.Context())
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	seg, ok := PickSegment(segs, counts)
	if !ok {
		httpx.Error(w, 503, "no ACTIVE segment")
		return
	}
	id, err := s.allocate(r.Context(), seg, req.RequestID)
	if err != nil {
		httpx.Error(w, 503, err.Error())
		return
	}
	// Create the account in the target DCN; on failure roll back the route row to keep routes and entities consistent
	if err := s.createInDCN(seg.Endpoint, id, req); err != nil {
		_, _ = s.db.Exec(`DELETE FROM account_route WHERE account_id = ?`, id)
		httpx.Error(w, 502, "dcn create account failed: "+err.Error())
		return
	}
	res := &LocateResult{AccountID: id, DCN: seg.DCN, Endpoint: seg.Endpoint}
	s.cache.Set(r.Context(), cacheKey(id), seg.DCN+"|"+seg.Endpoint, cacheTTL)
	httpx.JSON(w, 201, res)
}

func (s *Server) findByRequestID(ctx context.Context, requestID string) (*LocateResult, error) {
	res := &LocateResult{}
	err := s.db.QueryRowContext(ctx,
		`SELECT ar.account_id, ar.dcn, rs.endpoint FROM account_route ar
		 JOIN route_segment rs ON rs.dcn = ar.dcn WHERE ar.request_id = ?`, requestID).
		Scan(&res.AccountID, &res.DCN, &res.Endpoint)
	return res, err
}

func (s *Server) listSegments(ctx context.Context) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT dcn, seg_start, seg_end, endpoint, status FROM route_segment ORDER BY seg_start`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		var seg Segment
		if err := rows.Scan(&seg.DCN, &seg.SegStart, &seg.SegEnd, &seg.Endpoint, &seg.Status); err != nil {
			return nil, err
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

func (s *Server) accountCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dcn, COUNT(*) FROM account_route GROUP BY dcn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var dcn string
		var n int
		if err := rows.Scan(&dcn, &n); err != nil {
			return nil, err
		}
		out[dcn] = n
	}
	return out, rows.Err()
}

// allocate assigns the next account ID within the segment; retries up to 5 times on concurrent conflicts.
func (s *Server) allocate(ctx context.Context, seg Segment, requestID string) (int, error) {
	for i := 0; i < 5; i++ {
		var maxID sql.NullInt64
		if err := s.db.QueryRowContext(ctx,
			`SELECT MAX(account_id) FROM account_route WHERE dcn = ?`, seg.DCN).Scan(&maxID); err != nil {
			return 0, err
		}
		id, ok := NextAccountID(seg, int(maxID.Int64), maxID.Valid)
		if !ok {
			return 0, fmt.Errorf("segment %s full", seg.DCN)
		}
		var err error
		if requestID != "" {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO account_route (account_id, dcn, request_id) VALUES (?,?,?)`, id, seg.DCN, requestID)
		} else {
			_, err = s.db.ExecContext(ctx,
				`INSERT INTO account_route (account_id, dcn) VALUES (?,?)`, id, seg.DCN)
		}
		if err == nil {
			return id, nil
		}
		if !mysqlx.IsDuplicate(err) {
			return 0, err
		}
	}
	return 0, errors.New("account allocation failed after retries")
}

func (s *Server) createInDCN(endpoint string, id int, req openAccountRequest) error {
	body, _ := json.Marshal(map[string]any{
		"accountId": id, "name": req.Name, "initBalance": req.InitBalance,
	})
	resp, err := s.hc.Post(endpoint+"/accounts", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("dcn returned %d", resp.StatusCode)
	}
	return nil
}

func (s *Server) handleListRoutes(w http.ResponseWriter, r *http.Request) {
	segs, err := s.listSegments(r.Context())
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, segs)
}

// handleAddRoute adds a new segment (expansion entry point); a duplicate dcn primary key idempotently returns "exists".
// A new segment has no accounts yet, so there are no cache entries to invalidate; if segment modification is supported in the future, the route:* keys of affected accounts must be deleted.
func (s *Server) handleAddRoute(w http.ResponseWriter, r *http.Request) {
	var seg Segment
	if err := httpx.Decode(r, &seg); err != nil ||
		seg.DCN == "" || seg.Endpoint == "" || seg.SegStart <= 0 || seg.SegEnd < seg.SegStart {
		httpx.Error(w, 400, "dcn, endpoint and a valid segStart..segEnd range required")
		return
	}
	_, err := s.db.ExecContext(r.Context(),
		`INSERT INTO route_segment (dcn, seg_start, seg_end, endpoint, status) VALUES (?,?,?,?,'ACTIVE')`,
		seg.DCN, seg.SegStart, seg.SegEnd, seg.Endpoint)
	if mysqlx.IsDuplicate(err) {
		httpx.JSON(w, 200, map[string]string{"status": "exists", "dcn": seg.DCN})
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	seg.Status = "ACTIVE"
	httpx.JSON(w, 201, seg)
}
