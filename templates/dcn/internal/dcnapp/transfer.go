package dcnapp

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"

	"dcn/internal/platform/httpx"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

var (
	errDuplicate    = errors.New("duplicate movement")
	errInsufficient = errors.New("insufficient funds")
	errNotFound     = errors.New("account not found")
)

func newHTTPClient() *http.Client {
	// 必须大于 RMB 协调服务的同步等待窗口（10s）
	return &http.Client{Timeout: 15 * time.Second}
}

type transferRequest struct {
	TxID   string `json:"txId,omitempty"`
	FromID int    `json:"fromId"`
	ToID   int    `json:"toId"`
	Amount string `json:"amount"`
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := httpx.Decode(r, &req); err != nil || req.FromID <= 0 || req.ToID <= 0 {
		httpx.Error(w, 400, "fromId and toId required")
		return
	}
	amt, err := decimal.NewFromString(req.Amount)
	if err != nil || !amt.GreaterThan(decimal.Zero) {
		httpx.Error(w, 400, "amount must be a positive decimal")
		return
	}
	from, err := s.locate(req.FromID)
	if err != nil {
		httpx.Error(w, 404, "unknown from account")
		return
	}
	to, err := s.locate(req.ToID)
	if err != nil {
		httpx.Error(w, 404, "unknown to account")
		return
	}
	switch {
	case from.DCN == s.dcn && to.DCN == s.dcn:
		txID, err := s.localTransfer(req.FromID, req.ToID, amt)
		if errors.Is(err, errInsufficient) {
			httpx.Error(w, 422, "insufficient funds")
			return
		}
		if err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		httpx.JSON(w, 200, map[string]string{"status": "ok", "txId": txID})
	case from.DCN == s.dcn:
		s.submitRMB(w, req, to.DCN)
	default:
		// 接入层路由：源账户不在本单元，透明转发到其所属单元（非跨 DCN 业务通信）
		s.forward(w, req, from.Endpoint)
	}
}

type locateResult struct {
	AccountID int    `json:"accountId"`
	DCN       string `json:"dcn"`
	Endpoint  string `json:"endpoint"`
}

func (s *Server) locate(id int) (*locateResult, error) {
	resp, err := s.hc.Get(s.gns + "/locate?accountId=" + strconv.Itoa(id))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("not routed")
	}
	var res locateResult
	return &res, json.NewDecoder(resp.Body).Decode(&res)
}

// localTransfer 单库本地事务：条件更新防透支 + 双方 journal（uk_tx_acct 兜底）。
func (s *Server) localTransfer(fromID, toID int, amt decimal.Decimal) (string, error) {
	txID := "local-" + runx.RandHex(8)
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := applyMovement(tx, txID, fromID, "DEBIT", amt); err != nil {
		return "", err
	}
	if err := applyMovement(tx, txID, toID, "CREDIT", amt); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.publishEvent(txID, fromID, "DEBIT", amt)
	s.publishEvent(txID, toID, "CREDIT", amt)
	return txID, nil
}

// applyMovement 在一个本地事务内记 journal（幂等键）并变动余额。
func applyMovement(tx *sql.Tx, txID string, accountID int, dir string, amt decimal.Decimal) error {
	if _, err := tx.Exec(
		`INSERT INTO journal (tx_id, account_id, direction, amount) VALUES (?,?,?,?)`,
		txID, accountID, dir, amt.String()); err != nil {
		if mysqlx.IsDuplicate(err) {
			return errDuplicate
		}
		return err
	}
	switch dir {
	case "DEBIT":
		res, err := tx.Exec(
			`UPDATE account SET balance = balance - ? WHERE account_id = ? AND balance >= ?`,
			amt.String(), accountID, amt.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errInsufficient
		}
	case "CREDIT":
		res, err := tx.Exec(
			`UPDATE account SET balance = balance + ? WHERE account_id = ?`, amt.String(), accountID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNotFound
		}
	}
	return nil
}

// submitRMB 注册跨 DCN 总事务并同步等待协调结果。
func (s *Server) submitRMB(w http.ResponseWriter, req transferRequest, toDCN string) {
	payload := map[string]any{
		"type": "TRANSFER",
		"steps": []map[string]any{
			{"dcn": s.dcn, "action": "DEBIT", "accountId": req.FromID, "amount": req.Amount},
			{"dcn": toDCN, "action": "CREDIT", "accountId": req.ToID, "amount": req.Amount},
		},
	}
	if req.TxID != "" {
		payload["txId"] = req.TxID
	}
	body, _ := json.Marshal(payload)
	resp, err := s.hc.Post(s.rmb+"/transactions", "application/json", bytes.NewReader(body))
	if err != nil {
		httpx.Error(w, 502, "rmb unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	var result struct {
		TxID   string `json:"txId"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		httpx.Error(w, 502, "invalid rmb response")
		return
	}
	if result.Status == "COMMITTED" {
		httpx.JSON(w, 200, map[string]string{"status": "ok", "txId": result.TxID})
		return
	}
	httpx.JSON(w, 502, map[string]string{
		"error": "transfer not committed", "txId": result.TxID, "status": result.Status,
	})
}

// forward 把请求原样转发到目标单元（接入层职责）。
func (s *Server) forward(w http.ResponseWriter, req transferRequest, endpoint string) {
	body, _ := json.Marshal(req)
	resp, err := s.hc.Post(endpoint+"/transfer", "application/json", bytes.NewReader(body))
	if err != nil {
		httpx.Error(w, 502, "forward failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
