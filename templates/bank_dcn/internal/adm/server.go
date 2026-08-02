// Package adm implements the ADM zone service: it subscribes to balance change
// events from every DCN to maintain a global summary view, and provides global
// reports and bank-wide balance reconciliation (simulating the production T+x
// aggregation pipeline; second-level latency is acceptable).
package adm

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/shopspring/decimal"

	"bank_dcn/internal/contracts"
	"bank_dcn/internal/platform/httpx"
	"bank_dcn/internal/platform/metrics"
	"bank_dcn/internal/platform/mq"
	"bank_dcn/internal/platform/mysqlx"
)

// Server is the ADM service.
type Server struct {
	db  *sql.DB
	gns string
	hc  *http.Client
}

// NewServer constructs the ADM service.
func NewServer(db *sql.DB, gns string) *Server {
	return &Server{db: db, gns: gns, hc: &http.Client{Timeout: 2 * time.Second}}
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	metrics.Handle(mux, "adm", "GET /report/summary", http.HandlerFunc(s.handleSummary))
	metrics.Handle(mux, "adm", "GET /reconcile", http.HandlerFunc(s.handleReconcile))
	return mux
}

// DeclareAndConsume declares the event topology and starts consuming.
func (s *Server) DeclareAndConsume(mqc *mq.Conn) {
	mqc.DeclareFanoutExchange("adm.events")
	mqc.DeclareQueue("adm.events")
	mqc.Bind("adm.events", "adm.events", "")
	mqc.Consume("adm.events", s.handleEvent)
}

// handleEvent persists events idempotently and updates the global mirror (deduplicated by uk_event).
func (s *Server) handleEvent(body []byte) error {
	var evt contracts.BalanceEvent
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil
	}
	amt, err := decimal.NewFromString(evt.Amount)
	if err != nil {
		return nil
	}
	delta := amt
	if evt.Direction == "DEBIT" {
		delta = amt.Neg()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO event_log (tx_id, account_id, dcn, direction, amount) VALUES (?,?,?,?,?)`,
		evt.TxID, evt.AccountID, evt.DCN, evt.Direction, amt.String()); err != nil {
		if mysqlx.IsDuplicate(err) {
			return nil // duplicate event: ignore idempotently
		}
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO global_balance (account_id, dcn, balance) VALUES (?,?,?)
		 ON DUPLICATE KEY UPDATE balance = balance + VALUES(balance)`,
		evt.AccountID, evt.DCN, delta.String()); err != nil {
		return err
	}
	return tx.Commit()
}

type dcnStat struct {
	DCN      string `json:"dcn"`
	Accounts int    `json:"accounts"`
	Total    string `json:"totalBalance"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	var accounts int
	var total sql.NullString
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(balance), 0) FROM global_balance`).
		Scan(&accounts, &total); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	rows, err := s.db.Query(
		`SELECT dcn, COUNT(*), COALESCE(SUM(balance), 0) FROM global_balance GROUP BY dcn ORDER BY dcn`)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	defer rows.Close()
	per := []dcnStat{}
	for rows.Next() {
		var st dcnStat
		if err := rows.Scan(&st.DCN, &st.Accounts, &st.Total); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		per = append(per, st)
	}
	httpx.JSON(w, 200, map[string]any{
		"accounts": accounts, "totalBalance": total.String, "perDcn": per,
	})
}

type routeView struct {
	DCN      string `json:"dcn"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// handleReconcile compares the ADM summary against the sum of real-time balances from all DCNs.
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	resp, err := s.hc.Get(s.gns + "/routes")
	if err != nil {
		httpx.Error(w, 502, "gns unreachable: "+err.Error())
		return
	}
	var routes []routeView
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		resp.Body.Close()
		httpx.Error(w, 502, "invalid gns response")
		return
	}
	resp.Body.Close()

	dcnTotal := decimal.Zero
	per := []map[string]any{}
	errs := []string{}
	for _, rt := range routes {
		if rt.Status != "ACTIVE" {
			continue
		}
		rs, err := s.hc.Get(rt.Endpoint + "/internal/balance-sum")
		if err != nil {
			errs = append(errs, rt.DCN+": "+err.Error())
			continue
		}
		var v struct {
			Accounts   int    `json:"accounts"`
			BalanceSum string `json:"balanceSum"`
		}
		if err := json.NewDecoder(rs.Body).Decode(&v); err != nil {
			rs.Body.Close()
			errs = append(errs, rt.DCN+": invalid response")
			continue
		}
		rs.Body.Close()
		sum, err := decimal.NewFromString(v.BalanceSum)
		if err != nil {
			errs = append(errs, rt.DCN+": bad balanceSum")
			continue
		}
		dcnTotal = dcnTotal.Add(sum)
		per = append(per, map[string]any{
			"dcn": rt.DCN, "accounts": v.Accounts, "balanceSum": sum.String(),
		})
	}

	var admStr sql.NullString
	if err := s.db.QueryRow(
		`SELECT COALESCE(SUM(balance), 0) FROM global_balance`).Scan(&admStr); err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	admTotal, _ := decimal.NewFromString(admStr.String)
	consistent := len(errs) == 0 && admTotal.Equal(dcnTotal)
	log.Printf("reconcile: adm=%s dcn=%s consistent=%v", admTotal, dcnTotal, consistent)
	httpx.JSON(w, 200, map[string]any{
		"consistent": consistent,
		"admTotal":   admTotal.String(),
		"dcnTotal":   dcnTotal.String(),
		"perDcn":     per,
		"errors":     errs,
	})
}
