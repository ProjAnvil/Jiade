// Package dcnapp 实现 DCN 单元应用：账户业务、DCN 内本地转账、
// RMB 子事务执行与回执、ADM 变更事件上报。dcn01/02/03/04 同构，靠 env 区分。
package dcnapp

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/shopspring/decimal"

	"dcn/internal/contracts"
	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/ratelimit"
)

// Server 是一个 DCN 单元应用。
type Server struct {
	dcn string
	db  *sql.DB
	gns string
	rmb string
	mqc *mq.Conn
	rps float64
	hc  *http.Client
}

// NewServer 构造 DCN 应用。
func NewServer(dcn string, db *sql.DB, gns, rmb string, mqc *mq.Conn, rps float64) *Server {
	return &Server{dcn: dcn, db: db, gns: gns, rmb: rmb, mqc: mqc, rps: rps, hc: newHTTPClient()}
}

// Handler 返回带限流中间件的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /internal/balance-sum", s.handleBalanceSum)
	mux.HandleFunc("POST /transfer", s.handleTransfer)
	return ratelimit.New(s.rps).Middleware(mux)
}

// DeclareAndConsume 声明本单元的 RMB 队列并启动子事务消费。
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
	// 初始余额计入 ADM 全局镜像
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

// publishEvent 上报余额变更（at-most-once：提交后才发布，提交与发布之间崩溃会丢事件，
// 极端情况下 ADM 汇总与 DCN 短暂不一致，以 ADM reconcile 核对兜底；发布失败仅记日志）。
func (s *Server) publishEvent(txID string, accountID int, dir string, amt decimal.Decimal) {
	evt, _ := json.Marshal(contracts.BalanceEvent{
		TxID: txID, AccountID: accountID, DCN: s.dcn, Direction: dir, Amount: amt.String(),
	})
	if err := s.mqc.Publish("adm.events", "", evt); err != nil {
		log.Printf("adm event publish failed (tolerable): %v", err)
	}
}
