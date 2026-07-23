package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter assembles the customer read-only route.
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/customers", h.ListCustomers)
		r.Get("/customers/{cust_id}", h.GetCustomer)
		r.Get("/customers/{cust_id}/accounts", h.GetCustAccounts)
	})
	return r
}
