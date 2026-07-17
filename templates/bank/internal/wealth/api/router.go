package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 wealth 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/wealth/products", h.ListProducts)
		r.Get("/wealth/nav", h.ListNav)
		r.Get("/wealth/holdings", h.ListHoldings)
		r.Get("/wealth/holdings/{holding_id}/profile", h.GetHoldingProfile)
		r.Get("/wealth/orders", h.ListOrders)
		r.Get("/wealth/incomes", h.ListIncomes)
	})
	return r
}
