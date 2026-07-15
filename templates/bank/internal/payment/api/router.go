package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 payment 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/payments/transfers", h.ListTransfers)
		r.Get("/payments/transfers/{txn_id}", h.GetTransfer)
		r.Get("/payments/transfers/{txn_id}/parties", h.GetTransferParties)
		r.Get("/merchants/{merchant_id}", h.GetMerchant)
	})
	return r
}
