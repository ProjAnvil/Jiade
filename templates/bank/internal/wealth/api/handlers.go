// Package api is the transport layer of wealth service: http handlers + chi router.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/wealth/domain"
	"bank/internal/wealth/service"

	"github.com/go-chi/chi/v5"
)

// Handlers hold wealth read-only services. Production is done by Svc proxy repo; for single testing
// service.NewWealthService(fakeWealthRepo) injection.
type Handlers struct {
	Svc *service.WealthService
}

// Healthz Survival Check.
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListProducts lists financial products.
func (h *Handlers) ListProducts(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.ListProducts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthProductResp, 0, len(list))
	for _, p := range list {
		out = append(out, wealthProductResp{
			ProductCode: p.ProductCode, ProductName: p.ProductName, ProductType: p.ProductType,
			RiskLevel: p.RiskLevel, ExpectedReturn: p.ExpectedReturn, MinAmount: p.MinAmount.String(),
			TermDays: p.TermDays, StartBizDate: p.StartBizDate, EndBizDate: p.EndBizDate, Status: p.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"products": out})
}

// ListNav checks the daily net value by product/date range (query: product_code/from/to).
func (h *Handlers) ListNav(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := h.Svc.ListNav(r.Context(), q.Get("product_code"), q.Get("from"), q.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthNavResp, 0, len(list))
	for _, n := range list {
		out = append(out, wealthNavResp{
			ProductCode: n.ProductCode, BizDate: n.BizDate, Nav: n.Nav, AccumNav: n.AccumNav,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"navs": out})
}

// ListHoldings filters holdings by customer (query: cust_id/offset/limit).
func (h *Handlers) ListHoldings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListHoldings(r.Context(), q.Get("cust_id"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthHoldingResp, 0, len(list))
	for _, hd := range list {
		out = append(out, holdingRespOf(hd))
	}
	writeJSON(w, http.StatusOK, map[string]any{"holdings": out})
}

// ListOrders queries orders by customer/product/date range (query: cust_id/product_code/from/to/offset/limit).
func (h *Handlers) ListOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListOrders(r.Context(), q.Get("cust_id"), q.Get("product_code"), q.Get("from"), q.Get("to"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthOrderResp, 0, len(list))
	for _, o := range list {
		out = append(out, wealthOrderResp{
			OrderID: o.OrderID, BizDate: o.BizDate, CustID: o.CustID, ProductCode: o.ProductCode,
			AccountNo: o.AccountNo, OrderType: o.OrderType, Amount: o.Amount.String(),
			Share: o.Share, Nav: o.Nav, Status: o.Status,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"orders": out})
}

// ListIncomes checks the income by holding position/date range (query: holding_id/from/to/offset/limit).
func (h *Handlers) ListIncomes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListIncomes(r.Context(), q.Get("holding_id"), q.Get("from"), q.Get("to"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]wealthIncomeResp, 0, len(list))
	for _, inc := range list {
		out = append(out, wealthIncomeResp{
			IncomeID: inc.IncomeID, BizDate: inc.BizDate, HoldingID: inc.HoldingID,
			IncomeType: inc.IncomeType, Amount: inc.Amount.String(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"incomes": out})
}

// GetHoldingProfile checks the holding profile (cross-database federation JOIN). Does not exist and returns 404.
func (h *Handlers) GetHoldingProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "holding_id")
	p, err := h.Svc.HoldingProfile(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("持仓不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, wealthProfileResp{
		HoldingID: p.HoldingID, CustID: p.CustID, ProductCode: p.ProductCode,
		Share: p.Share, CurrentValue: p.CurrentValue.String(),
		CustName: p.CustName, CustType: p.CustType,
	})
}

// holdingRespOf holding → DTO.
func holdingRespOf(hd domain.WealthHolding) wealthHoldingResp {
	return wealthHoldingResp{
		HoldingID: hd.HoldingID, CustID: hd.CustID, AccountNo: hd.AccountNo, ProductCode: hd.ProductCode,
		Ccy: hd.Ccy, Share: hd.Share, Cost: hd.Cost.String(), CurrentValue: hd.CurrentValue.String(),
		BizDate: hd.BizDate,
	}
}

// --- DTO ---

type wealthProductResp struct {
	ProductCode    string `json:"product_code"`
	ProductName    string `json:"product_name"`
	ProductType    string `json:"product_type,omitempty"`
	RiskLevel      string `json:"risk_level,omitempty"`
	ExpectedReturn string `json:"expected_return,omitempty"`
	MinAmount      string `json:"min_amount"`
	TermDays       int    `json:"term_days,omitempty"`
	StartBizDate   string `json:"start_biz_date,omitempty"`
	EndBizDate     string `json:"end_biz_date,omitempty"`
	Status         string `json:"status,omitempty"`
}

type wealthNavResp struct {
	ProductCode string `json:"product_code"`
	BizDate     string `json:"biz_date"`
	Nav         string `json:"nav"`
	AccumNav    string `json:"accum_nav"`
}

type wealthHoldingResp struct {
	HoldingID    string `json:"holding_id"`
	CustID       string `json:"cust_id"`
	AccountNo    string `json:"account_no,omitempty"`
	ProductCode  string `json:"product_code"`
	Ccy          string `json:"ccy,omitempty"`
	Share        string `json:"share"`
	Cost         string `json:"cost"`
	CurrentValue string `json:"current_value"`
	BizDate      string `json:"biz_date,omitempty"`
}

type wealthOrderResp struct {
	OrderID     string `json:"order_id"`
	BizDate     string `json:"biz_date"`
	CustID      string `json:"cust_id"`
	ProductCode string `json:"product_code"`
	AccountNo   string `json:"account_no,omitempty"`
	OrderType   string `json:"order_type"`
	Amount      string `json:"amount"`
	Share       string `json:"share,omitempty"`
	Nav         string `json:"nav,omitempty"`
	Status      string `json:"status,omitempty"`
}

type wealthIncomeResp struct {
	IncomeID   string `json:"income_id"`
	BizDate    string `json:"biz_date"`
	HoldingID  string `json:"holding_id"`
	IncomeType string `json:"income_type,omitempty"`
	Amount     string `json:"amount"`
}

type wealthProfileResp struct {
	HoldingID    string `json:"holding_id"`
	CustID       string `json:"cust_id"`
	ProductCode  string `json:"product_code"`
	Share        string `json:"share"`
	CurrentValue string `json:"current_value"`
	CustName     string `json:"cust_name,omitempty"`
	CustType     string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
