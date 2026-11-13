package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter assembles the payment read-only route.
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/payments/transfers", h.ListTransfers)
		r.Get("/payments/transfers/{txn_id}", h.GetTransfer)
		r.Get("/payments/transfers/{txn_id}/parties", h.GetTransferParties)
		r.Get("/merchants/{merchant_id}", h.GetMerchant)
		// Payment-workflow saga endpoints (Task 7). Create, status, reverse.
		r.Post("/payments/workflows", h.CreatePaymentWorkflow)
		r.Get("/payments/workflows/{workflow_id}", h.GetPaymentWorkflow)
		r.Post("/payments/workflows/{workflow_id}/reverse", h.ReversePaymentWorkflow)
	})
	return r
}
