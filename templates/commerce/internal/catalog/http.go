package catalog

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"commerce/internal/platform/httpx"
)

func NewHandler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/products", func(w http.ResponseWriter, r *http.Request) {
		pageSize, err := parsePageSize(r)
		if err != nil {
			writeCatalogProblem(w, r, http.StatusBadRequest, "invalid_page_size")
			return
		}
		page, err := service.ListProducts(r.Context(), r.URL.Query().Get("cursor"), pageSize)
		if errors.Is(err, errInvalidCursor) {
			writeCatalogProblem(w, r, http.StatusBadRequest, "invalid_cursor")
			return
		}
		if err != nil {
			writeCatalogProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeCatalogJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v1/products/{id}", func(w http.ResponseWriter, r *http.Request) {
		product, err := service.GetProduct(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrProductNotFound) {
			writeCatalogProblem(w, r, http.StatusNotFound, "product_not_found")
			return
		}
		if err != nil {
			writeCatalogProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeCatalogJSON(w, http.StatusOK, product)
	})
	return mux
}

func parsePageSize(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("page_size")
	if raw == "" {
		return 0, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size <= 0 {
		return 0, errors.New("invalid page size")
	}
	return size, nil
}

func writeCatalogJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeCatalogProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	httpx.WriteProblem(w, httpx.Problem{
		Status: status, Code: code, Instance: r.URL.Path,
	})
}
