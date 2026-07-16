package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 reward 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/reward/points-accounts", h.ListPointsAccts)
		r.Get("/reward/points-accounts/{cust_id}", h.GetPointsAcct)
		r.Get("/reward/customers/{cust_id}/coupons", h.ListCoupons)
		r.Get("/reward/customers/{cust_id}/profile", h.GetProfile)
	})
	return r
}
