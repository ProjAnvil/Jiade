package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/accounts/{account_no}", h.GetAccount)
		r.Get("/accounts/{account_no}/balance", h.GetBalance)
		r.Get("/txns", h.ListTxns)
		r.Get("/ledger", h.GetLedger)
	})
	return r
}

// chiURLParam 从 chi 路由上下文取路径参数。
func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}
