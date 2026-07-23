package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter assembles a read-only route.
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/accounts/{account_no}", h.GetAccount)
		r.Get("/accounts/{account_no}/balance", h.GetBalance)
		r.Get("/txns", h.ListTxns)
		r.Get("/ledger", h.GetLedger)
		r.Post("/txns", h.PostTxn)                                 // B-3 Accounting
		r.Post("/vouchers/{voucher_no}/reverse", h.ReverseVoucher) // B-3 correction
	})
	return r
}

// chiURLParam takes path parameters from the chi routing context.
func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}
