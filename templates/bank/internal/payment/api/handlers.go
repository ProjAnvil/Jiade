// Package api is the transport layer of payment service: http handlers + chi router.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/payment/domain"
	"bank/internal/payment/service"

	"github.com/go-chi/chi/v5"
)

// Handlers hold the payment read-only service. Production is done by Svc proxy repo; for single testing
// service.NewPaymentService(fakePayRepo) injection.
type Handlers struct {
	Svc *service.PaymentService
}

// Healthz Survival Check.
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ListTransfers filter and paginate by account/date (query: account_no/from/to/limit/offset).
func (h *Handlers) ListTransfers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	ts, err := h.Svc.ListTransfers(r.Context(), q.Get("account_no"), q.Get("from"), q.Get("to"), limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]transferResp, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTransferResp(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": out})
}

// GetTransfer checks a single transfer. Does not exist and returns 404.
func (h *Handlers) GetTransfer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "txn_id")
	t, err := h.Svc.GetTransfer(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("转账不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, toTransferResp(t))
}

// GetTransferParties checks the transfer parties (cross-database federated JOIN).
func (h *Handlers) GetTransferParties(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "txn_id")
	p, err := h.Svc.GetParties(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("转账不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, partiesResp{
		TxnID: p.TxnID, Amount: p.Amount.String(), Ccy: p.Ccy, BizDate: p.BizDate,
		OutAccount: p.OutAccount, OutCustName: p.OutCustName,
		InAccount: p.InAccount, InCustName: p.InCustName,
	})
}

// GetMerchant Check merchants. Does not exist and returns 404.
func (h *Handlers) GetMerchant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "merchant_id")
	m, err := h.Svc.GetMerchant(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("商户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, merchantResp{
		MerchantID: m.MerchantID, MerchantName: m.MerchantName, MCC: m.MCC,
		Region: m.Region, Status: m.Status, CreateBizDate: m.CreateBizDate,
	})
}

// --- DTO ---

type transferResp struct {
	TxnID      string `json:"txn_id"`
	BizDate    string `json:"biz_date"`
	OutAccount string `json:"out_account"`
	InAccount  string `json:"in_account"`
	Amount     string `json:"amount"`
	Ccy        string `json:"ccy"`
	Fee        string `json:"fee"`
	Channel    string `json:"channel,omitempty"`
	Summary    string `json:"summary,omitempty"`
}

type partiesResp struct {
	TxnID       string `json:"txn_id"`
	Amount      string `json:"amount"`
	Ccy         string `json:"ccy"`
	BizDate     string `json:"biz_date"`
	OutAccount  string `json:"out_account"`
	OutCustName string `json:"out_cust_name"`
	InAccount   string `json:"in_account"`
	InCustName  string `json:"in_cust_name"`
}

type merchantResp struct {
	MerchantID    string `json:"merchant_id"`
	MerchantName  string `json:"merchant_name"`
	MCC           string `json:"mcc"`
	Region        string `json:"region"`
	Status        string `json:"status"`
	CreateBizDate string `json:"create_biz_date"`
}

// toTransferResp Convert domain.Transfer to DTO; amount serialized to NUMERIC text via Money.String().
func toTransferResp(t domain.Transfer) transferResp {
	return transferResp{
		TxnID: t.TxnID, BizDate: t.BizDate, OutAccount: t.OutAccount, InAccount: t.InAccount,
		Amount: t.Amount.String(), Ccy: t.Ccy, Fee: t.Fee.String(),
		Channel: t.Channel, Summary: t.Summary,
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
