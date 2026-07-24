package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestCheckoutSKUSnapshotReturnsImmutableSaleFields(t *testing.T) {
	store := &catalogStoreStub{}
	store.snapshot = CheckoutSnapshot{
		ProductID: "PROD-1", SKU: "SKU-1", ProductTitle: "Product",
		VariantTitle: "Black / S", Title: "Product — Black / S",
		Status: "active", AvailableForSale: true, UnitPriceMinor: 1236,
		Currency: "CNY", WeightGrams: 220,
		Attributes: map[string]any{"color": "black", "size": "S"},
	}
	handler := NewHandler(NewService(store))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(
		http.MethodGet, "/internal/v1/catalog/skus/SKU-1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var snapshot CheckoutSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.ProductID != "PROD-1" || snapshot.SKU != "SKU-1" ||
		snapshot.UnitPriceMinor != 1236 || !snapshot.AvailableForSale ||
		snapshot.Attributes["color"] != "black" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}

func TestCheckoutSKUSnapshotMapsUnknownAndInactiveSKU(t *testing.T) {
	store := &catalogStoreStub{snapshotErr: ErrSKUNotFound}
	handler := NewHandler(NewService(store))
	missing := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/catalog/skus/MISSING", nil)
	handler.ServeHTTP(missing, request)
	assertCatalogProblem(t, missing, http.StatusNotFound, "sku_not_found", request.URL.Path)

	store.snapshotErr = nil
	store.snapshot = CheckoutSnapshot{
		ProductID: "PROD-1", SKU: "SKU-INACTIVE", ProductTitle: "Product",
		VariantTitle: "Retired", Title: "Product — Retired", Status: "inactive",
		Currency: "CNY", UnitPriceMinor: 100,
	}
	inactive := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/internal/v1/catalog/skus/SKU-INACTIVE", nil)
	handler.ServeHTTP(inactive, request)
	assertCatalogProblem(t, inactive, http.StatusUnprocessableEntity, "sku_not_saleable", request.URL.Path)
}

// TestCheckoutSKUSnapshotAcceptsLegacyAndWebChannels locks the controller's
// saleability gate: a SKU with no web price_list rows is priced via the legacy
// fallback (channel="legacy") and MUST remain saleable. The dev seed produces
// NO variant_price/price_list rows, so every seeded SKU relies on this path.
// A SKU advertised on an unsupported channel (e.g. "retail") is rejected.
func TestCheckoutSKUSnapshotAcceptsLegacyAndWebChannels(t *testing.T) {
	base := CheckoutSnapshot{
		ProductID: "PROD-1", SKU: "SKU-1", ProductTitle: "Product",
		VariantTitle: "Black / S", Status: "active",
		UnitPriceMinor: 1999, Currency: "USD", WeightGrams: 220,
		Attributes: map[string]any{"color": "black"},
	}

	for _, channel := range []string{"", "legacy", "web"} {
		store := &catalogStoreStub{}
		snapshot := base
		snapshot.SKU = "SKU-" + channel
		snapshot.Channel = channel
		store.snapshot = snapshot
		handler := NewHandler(NewService(store))
		response := httptest.NewRecorder()
		path := "/internal/v1/catalog/skus/" + snapshot.SKU
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("channel=%q expected 200, got status=%d body=%s",
				channel, response.Code, response.Body.String())
		}
		var got CheckoutSnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatalf("channel=%q decode: %v", channel, err)
		}
		if !got.AvailableForSale {
			t.Fatalf("channel=%q AvailableForSale=false; legacy fallback must be saleable",
				channel)
		}
	}

	// An unsupported channel is not saleable. This guards the controller's
	// allow-list (web/legacy/empty) from silently widening.
	store := &catalogStoreStub{}
	retail := base
	retail.SKU = "SKU-RETAIL"
	retail.Channel = "retail"
	store.snapshot = retail
	handler := NewHandler(NewService(store))
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/catalog/skus/SKU-RETAIL", nil)
	handler.ServeHTTP(response, request)
	assertCatalogProblem(t, response, http.StatusUnprocessableEntity, "sku_not_saleable", request.URL.Path)
}

func TestCheckoutSnapshotStoreQueriesOwnedCatalogTablesBySKU(t *testing.T) {
	source, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, fragment := range []string{
		"FROM variant v", "JOIN product p", "WHERE v.sku = $1",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("store.go missing SKU snapshot SQL %q", fragment)
		}
	}
}

type catalogStoreStub struct {
	products    []Product
	details     map[string]Product
	snapshot    CheckoutSnapshot
	snapshotErr error
	lastAfter   string
	lastLimit   int
	err         error
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

func (store *catalogStoreStub) GetCheckoutSnapshot(context.Context, string) (CheckoutSnapshot, error) {
	return store.snapshot, store.snapshotErr
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
