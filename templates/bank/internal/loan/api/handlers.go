// Package api is the transport layer of loan service: http handlers + chi router.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/loan/domain"
	"bank/internal/loan/service"

	"github.com/go-chi/chi/v5"
)

// Handlers hold loan read-only services. Production is done by Svc proxy repo; for single testing
// service.NewLoanService(fakeLoanRepo) injection.
type Handlers struct {
	Svc *service.LoanService
}

// Healthz Survival Check.
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListProducts lists loan products.
func (h *Handlers) ListProducts(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.ListProducts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanProductResp, 0, len(list))
	for _, p := range list {
		out = append(out, loanProductResp{
			ProductCode: p.ProductCode, ProductName: p.ProductName, LoanType: p.LoanType,
			MinRate: p.MinRate, MaxRate: p.MaxRate, MaxTerm: p.MaxTerm,
			MaxAmount: p.MaxAmount.String(), Status: p.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

// ListAccounts filters IOUs by product/status (query: product_code/status/offset/limit).
func (h *Handlers) ListAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListAccounts(r.Context(), q.Get("product_code"), q.Get("status"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanAccountResp, 0, len(list))
	for _, a := range list {
		out = append(out, accountRespOf(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

// GetAccount checks a single IOU. Does not exist and returns 404.
func (h *Handlers) GetAccount(w http.ResponseWriter, r *http.Request) {
	no := chi.URLParam(r, "loan_no")
	a, err := h.Svc.GetAccount(r.Context(), no)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("借据不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, accountRespOf(a))
}

// ListBalances checks daily balance snapshots by date range (query: from/to/loan_no/offset/limit).
func (h *Handlers) ListBalances(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListBalances(r.Context(), q.Get("from"), q.Get("to"), q.Get("loan_no"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanBalanceResp, 0, len(list))
	for _, b := range list {
		out = append(out, loanBalanceResp{
			LoanNo: b.LoanNo, BizDate: b.BizDate,
			PrincipalBalance: b.PrincipalBalance.String(), InterestReceivable: b.InterestReceivable.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"balances": out})
}

// ListOverdue checks overdue items by five-level classification/date range (query: overdue_class/from/to/offset/limit).
func (h *Handlers) ListOverdue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListOverdue(r.Context(), q.Get("overdue_class"), q.Get("from"), q.Get("to"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]loanOverdueResp, 0, len(list))
	for _, o := range list {
		out = append(out, loanOverdueResp{
			OverdueID: o.OverdueID, BizDate: o.BizDate, LoanNo: o.LoanNo,
			OverdueDays: o.OverdueDays, OverdueClass: o.OverdueClass, OverdueAmount: o.OverdueAmount.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"overdues": out})
}

// GetProfile checks the IOU file (cross-database federated JOIN).
func (h *Handlers) GetProfile(w http.ResponseWriter, r *http.Request) {
	no := chi.URLParam(r, "loan_no")
	p, err := h.Svc.Profile(r.Context(), no)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("借据不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, loanProfileResp{
		LoanNo: p.LoanNo, CustID: p.CustID, Principal: p.Principal.String(),
		Balance: p.Balance.String(), Rate: p.Rate, Status: p.Status,
		CustName: p.CustName, CustType: p.CustType,
	})
}

// accountRespOf IOU → DTO.
func accountRespOf(a domain.LoanAccount) loanAccountResp {
	return loanAccountResp{
		LoanNo: a.LoanNo, CustID: a.CustID, ProductCode: a.ProductCode, Ccy: a.Ccy,
		Principal: a.Principal.String(), Balance: a.Balance.String(), Rate: a.Rate,
		StartBizDate: a.StartBizDate, MatureDate: a.MatureDate, TermMonths: a.TermMonths,
		Status: a.Status, GuaranteeType: a.GuaranteeType, BranchCode: a.BranchCode,
	}
}

// --- DTO ---

type loanProductResp struct {
	ProductCode string `json:"product_code"`
	ProductName string `json:"product_name"`
	LoanType    string `json:"loan_type,omitempty"`
	MinRate     string `json:"min_rate,omitempty"`
	MaxRate     string `json:"max_rate,omitempty"`
	MaxTerm     int    `json:"max_term,omitempty"`
	MaxAmount   string `json:"max_amount"`
	Status      string `json:"status,omitempty"`
}

type loanAccountResp struct {
	LoanNo        string `json:"loan_no"`
	CustID        string `json:"cust_id"`
	ProductCode   string `json:"product_code"`
	Ccy           string `json:"ccy,omitempty"`
	Principal     string `json:"principal"`
	Balance       string `json:"balance"`
	Rate          string `json:"rate"`
	StartBizDate  string `json:"start_biz_date,omitempty"`
	MatureDate    string `json:"mature_date,omitempty"`
	TermMonths    int    `json:"term_months,omitempty"`
	Status        string `json:"status,omitempty"`
	GuaranteeType string `json:"guarantee_type,omitempty"`
	BranchCode    string `json:"branch_code,omitempty"`
}

type loanBalanceResp struct {
	LoanNo             string `json:"loan_no"`
	BizDate            string `json:"biz_date"`
	PrincipalBalance   string `json:"principal_balance"`
	InterestReceivable string `json:"interest_receivable"`
}

type loanOverdueResp struct {
	OverdueID     string `json:"overdue_id"`
	BizDate       string `json:"biz_date"`
	LoanNo        string `json:"loan_no"`
	OverdueDays   int    `json:"overdue_days"`
	OverdueClass  string `json:"overdue_class"`
	OverdueAmount string `json:"overdue_amount"`
}

type loanProfileResp struct {
	LoanNo    string `json:"loan_no"`
	CustID    string `json:"cust_id"`
	Principal string `json:"principal"`
	Balance   string `json:"balance"`
	Rate      string `json:"rate"`
	Status    string `json:"status,omitempty"`
	CustName  string `json:"cust_name,omitempty"`
	CustType  string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
