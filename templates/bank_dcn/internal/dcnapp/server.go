// Package dcnapp implements the DCN unit application: account operations, intra-DCN
// local transfers, RMB sub-transaction execution and receipts, and ADM change event
// reporting. dcn01/02/03/04 are homogeneous and distinguished by env.
package dcnapp

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"

	"bank_dcn/internal/contracts"
	"bank_dcn/internal/platform/httpx"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mq"
	"bank_dcn/internal/platform/mysqlx"
	"bank_dcn/internal/platform/ratelimit"
)

// Server is a DCN unit application.
type Server struct {
	dcn  string
	db   *sql.DB
	gns  string
	rmb  string
	mqc  *mq.Conn
	rps  float64
	rate decimal.Decimal
	hc   *http.Client

	publishFn func(exchange, key string, body []byte) error // defaults to mqc.Publish; injectable in tests
}

// NewServer constructs a DCN application; rate is the daily rate for end-of-day interest.
func NewServer(dcn string, db *sql.DB, gns, rmb string, mqc *mq.Conn, rps float64, rate decimal.Decimal) *Server {
	return &Server{dcn: dcn, db: db, gns: gns, rmb: rmb, mqc: mqc, rps: rps, rate: rate, hc: newHTTPClient(),
		publishFn: mqc.Publish}
}

// Handler returns routes wrapped with rate limiting and metrics; /metrics is mounted via
// metrics.Mount outside the rate limiter.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, s.dcn, "POST /accounts", http.HandlerFunc(s.handleCreateAccount))
	metrics.Handle(mux, s.dcn, "GET /accounts/{id}/balance", http.HandlerFunc(s.handleBalance))
	metrics.Handle(mux, s.dcn, "GET /internal/balance-sum", http.HandlerFunc(s.handleBalanceSum))
	metrics.Handle(mux, s.dcn, "POST /transfer", http.HandlerFunc(s.handleTransfer))
	metrics.Handle(mux, s.dcn, "POST /internal/batch/interest", http.HandlerFunc(s.handleInterestBatch))
	return metrics.Mount(ratelimit.New(s.rps).Middleware(mux))
}

// DeclareAndConsume declares this unit's RMB queue and starts consuming sub-transactions.
func (s *Server) DeclareAndConsume() {
	s.mqc.DeclareTopicExchange("rmb.steps")
	s.mqc.DeclareQueue("rmb.steps." + s.dcn)
	s.mqc.Bind("rmb.steps."+s.dcn, "rmb.steps", "step."+s.dcn)
	s.mqc.DeclareFanoutExchange("adm.events")
	s.mqc.Consume("rmb.steps."+s.dcn, s.handleStep)
}

type createAccountRequest struct {
	AccountID   int    `json:"accountId"`
	Name        string `json:"name"`
	InitBalance string `json:"initBalance"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	var req createAccountRequest
	if err := httpx.Decode(r, &req); err != nil || req.AccountID <= 0 || req.Name == "" {
		httpx.Error(w, 400, "accountId and name required")
		return
	}
	bal, err := decimal.NewFromString(req.InitBalance)
	if err != nil || bal.IsNegative() {
		httpx.Error(w, 400, "initBalance must be a non-negative decimal")
		return
	}
	_, err = s.db.Exec(`INSERT INTO account (account_id, name, balance) VALUES (?,?,?)`,
		req.AccountID, req.Name, bal.String())
	if mysqlx.IsDuplicate(err) {
		httpx.JSON(w, 200, map[string]any{"accountId": req.AccountID, "status": "exists"})
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	// Count the initial balance into the ADM global mirror
	if bal.GreaterThan(decimal.Zero) {
		s.publishEvent("init-"+strconv.Itoa(req.AccountID), req.AccountID, "CREDIT", bal)
	}
	httpx.JSON(w, 201, map[string]any{"accountId": req.AccountID, "status": "created"})
}

func (s *Server) handleBalance(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		httpx.Error(w, 400, "invalid account id")
		return
	}
	var bal string
	err = s.db.QueryRow(`SELECT balance FROM account WHERE account_id = ?`, id).Scan(&bal)
	if err == sql.ErrNoRows {
		httpx.Error(w, 404, "account not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, map[string]any{"accountId": id, "balance": bal})
}

func (s *Server) handleBalanceSum(w http.ResponseWriter, r *http.Request) {
	var n int
	var sum sql.NullString
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(balance), 0) FROM account`).Scan(&n, &sum); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	httpx.JSON(w, 200, map[string]any{"dcn": s.dcn, "accounts": n, "balanceSum": sum.String})
}

// publishEvent reports a balance change (at-most-once: published only after commit, so a
// crash between commit and publish loses the event; in extreme cases the ADM aggregate
// may briefly diverge from the DCN, backstopped by the ADM reconcile check; publish
// failures are only logged).
func (s *Server) publishEvent(txID string, accountID int, dir string, amt decimal.Decimal) {
	evt, _ := json.Marshal(contracts.BalanceEvent{
		TxID: txID, AccountID: accountID, DCN: s.dcn, Direction: dir, Amount: amt.String(),
	})
	if s.publishFn == nil {
		return // not published when not injected in unit tests
	}
	if err := s.publishFn("adm.events", "", evt); err != nil {
		log.Printf("adm event publish failed (tolerable): %v", err)
	}
}
