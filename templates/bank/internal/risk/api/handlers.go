// Package api is the transport layer of risk service: http handlers + chi router.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/risk/service"

	"github.com/go-chi/chi/v5"
)

// Handlers hold risk read-only services.
type Handlers struct {
	Svc *service.RiskService
}

// Healthz Survival Check.
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListEvents is filtered and paginated by conditions (query: from/to/rule_id/action/offset/limit).
func (h *Handlers) ListEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListEvents(r.Context(), q.Get("from"), q.Get("to"), q.Get("rule_id"), q.Get("action"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]eventResp, 0, len(list))
	for _, e := range list {
		out = append(out, eventResp{
			EventID: e.EventID, BizDate: e.BizDate, CustID: e.CustID, RuleID: e.RuleID,
			RiskScore: e.RiskScore, ActionTaken: e.ActionTaken, Summary: e.Summary,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// GetEvent Check event details (cross-database federation JOIN). Does not exist and returns 404.
func (h *Handlers) GetEvent(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "event_id")
	d, err := h.Svc.Event(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("风控事件不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, eventDetailResp{
		eventResp: eventResp{
			EventID: d.EventID, BizDate: d.BizDate, CustID: d.CustID, RuleID: d.RuleID,
			RiskScore: d.RiskScore, ActionTaken: d.ActionTaken, Summary: d.Summary,
		},
		CustName: d.CustName, CustType: d.CustType,
	})
}

// ListRules lists risk control rules (static).
func (h *Handlers) ListRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.Svc.Rules(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

// ListBlacklists filters blacklists by customer (query: cust_id/offset/limit).
func (h *Handlers) ListBlacklists(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.Blacklists(r.Context(), q.Get("cust_id"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"blacklists": list})
}

// --- DTO ---

type eventResp struct {
	EventID     string `json:"event_id"`
	BizDate     string `json:"biz_date"`
	CustID      string `json:"cust_id,omitempty"`
	RuleID      string `json:"rule_id,omitempty"`
	RiskScore   string `json:"risk_score,omitempty"`
	ActionTaken string `json:"action_taken,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type eventDetailResp struct {
	eventResp
	CustName string `json:"cust_name,omitempty"`
	CustType string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
