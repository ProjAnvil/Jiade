package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配 risk 只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/risk/events", h.ListEvents)
		r.Get("/risk/events/{event_id}", h.GetEvent)
		r.Get("/risk/rules", h.ListRules)
		r.Get("/risk/blacklists", h.ListBlacklists)
	})
	return r
}
