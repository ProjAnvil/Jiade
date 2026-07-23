// Package api is the transport layer of customer service: http handlers + chi router.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/customer/service"

	"github.com/go-chi/chi/v5"
)

// Handlers hold the customer read-only service. Production is done by Svc proxy repo; for single testing
// service.NewCustomerService(fakeStore) injection.
type Handlers struct {
	Svc *service.CustomerService
}

// Healthz Survival Check.
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GetCustomer checks a single customer. Does not exist and returns 404.
func (h *Handlers) GetCustomer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	c, err := h.Svc.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("客户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, customerResp{
		CustID: c.CustID, CustType: string(c.CustType), Name: c.Name,
		CertType: c.CertType, Gender: c.Gender, Birthday: c.Birthday,
		Nationality: c.Nationality, RiskLevel: c.RiskLevel, KYCStatus: c.KYCStatus,
		CreateBizDate: c.CreateBizDate,
	})
}

// ListCustomers filter and page by type/kyc (query: type/kyc_status/offset/limit).
func (h *Handlers) ListCustomers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.List(r.Context(), q.Get("type"), q.Get("kyc_status"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]customerResp, 0, len(list))
	for _, c := range list {
		out = append(out, customerResp{
			CustID: c.CustID, CustType: string(c.CustType), Name: c.Name,
			KYCStatus: c.KYCStatus, RiskLevel: c.RiskLevel, CreateBizDate: c.CreateBizDate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"customers": out})
}

// GetCustAccounts checks customer related accounts (cross-database federated query).
func (h *Handlers) GetCustAccounts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	accts, err := h.Svc.Accounts(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]accountResp, 0, len(accts))
	for _, a := range accts {
		out = append(out, accountResp{
			AccountNo: a.AccountNo, Ccy: a.Ccy, Status: a.Status,
			OpenBizDate: a.OpenBizDate, Branch: a.BranchCode, Role: a.Role,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// --- DTO ---

type customerResp struct {
	CustID        string `json:"cust_id"`
	CustType      string `json:"cust_type"`
	Name          string `json:"name"`
	CertType      string `json:"cert_type,omitempty"`
	Gender        string `json:"gender,omitempty"`
	Birthday      string `json:"birthday,omitempty"`
	Nationality   string `json:"nationality,omitempty"`
	RiskLevel     string `json:"risk_level,omitempty"`
	KYCStatus     string `json:"kyc_status,omitempty"`
	CreateBizDate string `json:"create_biz_date,omitempty"`
}

type accountResp struct {
	AccountNo   string `json:"account_no"`
	Ccy         string `json:"ccy"`
	Status      string `json:"status"`
	OpenBizDate string `json:"open_biz_date"`
	Branch      string `json:"branch_code,omitempty"`
	Role        string `json:"role,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
