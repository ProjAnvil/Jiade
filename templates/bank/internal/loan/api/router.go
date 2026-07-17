package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 loan 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/loan/products", h.ListProducts)
		r.Get("/loan/accounts", h.ListAccounts)
		r.Get("/loan/accounts/{loan_no}", h.GetAccount)
		r.Get("/loan/accounts/{loan_no}/profile", h.GetProfile)
		r.Get("/loan/balances", h.ListBalances)
		r.Get("/loan/overdue", h.ListOverdue)
	})
	return r
}
