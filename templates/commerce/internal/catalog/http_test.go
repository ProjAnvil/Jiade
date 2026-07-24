package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"commerce/internal/platform/httpx"
)

func TestProductListUsesOpaqueCursorAndCapsPageSize(t *testing.T) {
	store := &catalogStoreStub{
		products: []Product{
			{ID: "PROD-1", Title: "One", Brand: "Brand", Category: "Category", Status: "active"},
			{ID: "PROD-2", Title: "Two", Brand: "Brand", Category: "Category", Status: "active"},
		},
	}
	handler := NewHandler(NewService(store))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/products?page_size=1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var page ProductPage
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != "PROD-1" || page.NextCursor == "" ||
		strings.Contains(page.NextCursor, "PROD-1") {
		t.Fatalf("unexpected first page: %+v", page)
	}

	second := httptest.NewRecorder()
	path := "/api/v1/products?page_size=999&cursor=" + page.NextCursor
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, path, nil))
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}
	if store.lastAfter != "PROD-1" || store.lastLimit != MaxPageSize+1 {
		t.Fatalf("after=%q limit=%d", store.lastAfter, store.lastLimit)
	}
}

func TestProductListRejectsInvalidCursorAsProblem(t *testing.T) {
	handler := NewHandler(NewService(&catalogStoreStub{}))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products?cursor=not-a-valid-cursor", nil)
	handler.ServeHTTP(response, request)
	assertCatalogProblem(t, response, http.StatusBadRequest, "invalid_cursor", request.URL.Path)
}

func TestProductDetailAndMissingProduct(t *testing.T) {
	store := &catalogStoreStub{details: map[string]Product{
		"PROD-1": {
			ID: "PROD-1", Title: "One", Description: "Description", Brand: "Brand",
			Category: "Category", Status: "active",
			Variants: []Variant{{SKU: "SKU-1", Title: "Red", PriceMinor: 1234, Currency: "CNY"}},
		},
	}}
	handler := NewHandler(NewService(store))

	found := httptest.NewRecorder()
	handler.ServeHTTP(found, httptest.NewRequest(http.MethodGet, "/api/v1/products/PROD-1", nil))
	if found.Code != http.StatusOK || !strings.Contains(found.Body.String(), `"sku":"SKU-1"`) {
		t.Fatalf("found status=%d body=%s", found.Code, found.Body.String())
	}

	missing := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products/missing", nil)
	handler.ServeHTTP(missing, request)
	assertCatalogProblem(t, missing, http.StatusNotFound, "product_not_found", request.URL.Path)
}

func TestCatalogResponsesIncludeRequestAndInstanceHeaders(t *testing.T) {
	inner := NewHandler(NewService(&catalogStoreStub{}))
	handler := httpx.NewServer(httpx.ServerConfig{
		Service: "catalog", Instance: "catalog-2", Handler: inner,
	}).Handler()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.Header.Set("X-Request-ID", "request-123")
	handler.ServeHTTP(response, request)
	if response.Header().Get("X-Request-ID") != "request-123" ||
		response.Header().Get("X-Service-Instance") != "catalog-2" {
		t.Fatalf("headers=%v", response.Header())
	}
}

type catalogStoreStub struct {
	products  []Product
	details   map[string]Product
	lastAfter string
	lastLimit int
	err       error
}

func (store *catalogStoreStub) ListProducts(_ context.Context, after string, limit int) ([]Product, error) {
	store.lastAfter, store.lastLimit = after, limit
	if store.err != nil {
		return nil, store.err
	}
	start := 0
	if after != "" {
		for start < len(store.products) && store.products[start].ID <= after {
			start++
		}
	}
	end := start + limit
	if end > len(store.products) {
		end = len(store.products)
	}
	return append([]Product(nil), store.products[start:end]...), nil
}

func (store *catalogStoreStub) GetProduct(_ context.Context, id string) (Product, error) {
	if store.err != nil {
		return Product{}, store.err
	}
	product, ok := store.details[id]
	if !ok {
		return Product{}, ErrProductNotFound
	}
	return product, nil
}

func assertCatalogProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code, instance string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status=%d type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var problem httpx.Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != code || problem.Instance != instance {
		t.Fatalf("problem=%+v", problem)
	}
}

var _ Store = (*catalogStoreStub)(nil)
