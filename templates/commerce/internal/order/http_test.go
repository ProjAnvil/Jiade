package order

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"commerce/internal/platform/httpx"
)

func TestCartHTTPCreateGetAndVersionedLineMutations(t *testing.T) {
	fixture := newCheckoutFixture()
	handler := NewHandler(fixture.service)

	created := postOrderJSON(handler, http.MethodPost, "/api/v1/carts",
		`{"customer_id":"CUS-2","currency":"CNY"}`, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var cart Cart
	if err := json.Unmarshal(created.Body.Bytes(), &cart); err != nil {
		t.Fatal(err)
	}
	if cart.ID == "" || cart.Version != 1 || cart.Status != CartActive {
		t.Fatalf("created cart=%+v", cart)
	}

	added := postOrderJSON(handler, http.MethodPost, "/api/v1/carts/"+cart.ID+"/items",
		`{"sku":"SKU-2","quantity":2,"expected_version":1}`, "")
	if added.Code != http.StatusOK {
		t.Fatalf("add status=%d body=%s", added.Code, added.Body.String())
	}
	var afterAdd Cart
	if err := json.Unmarshal(added.Body.Bytes(), &afterAdd); err != nil {
		t.Fatal(err)
	}
	if afterAdd.Version != 2 || len(afterAdd.Lines) != 1 || added.Header().Get("ETag") != `"2"` {
		t.Fatalf("after add=%+v ETag=%q", afterAdd, added.Header().Get("ETag"))
	}

	stale := postOrderJSON(handler, http.MethodPatch, "/api/v1/carts/"+cart.ID+"/items/SKU-2",
		`{"quantity":3,"expected_version":1}`, "")
	assertOrderProblem(t, stale, http.StatusConflict, "cart_version_conflict")

	removed := postOrderJSON(handler, http.MethodDelete, "/api/v1/carts/"+cart.ID+"/items/SKU-2",
		`{"expected_version":2}`, "")
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), `"version":3`) ||
		!strings.Contains(removed.Body.String(), `"lines":[]`) {
		t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
	}

	got := httptest.NewRecorder()
	handler.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/api/v1/carts/"+cart.ID, nil))
	if got.Code != http.StatusOK || got.Body.String() != removed.Body.String() {
		t.Fatalf("get status=%d body=%s removed=%s", got.Code, got.Body.String(), removed.Body.String())
	}
}

func TestCheckoutHTTPRequiresJSONAndIdempotencyAndPropagatesHeaders(t *testing.T) {
	fixture := newCheckoutFixture()
	handler := NewHandler(fixture.service)

	noKey := postOrderJSON(handler, http.MethodPost, "/api/v1/checkouts",
		`{"cart_id":"CART-1","address_id":"ADDR-1"}`, "")
	assertOrderProblem(t, noKey, http.StatusBadRequest, "idempotency_key_required")

	request := httptest.NewRequest(http.MethodPost, "/api/v1/checkouts",
		strings.NewReader(`{"cart_id":"CART-1","address_id":"ADDR-1"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "checkout-http-1")
	request.Header.Set("X-Request-ID", "request-http-1")
	request.Header.Set("traceparent", "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("checkout status=%d body=%s", response.Code, response.Body.String())
	}
	if fixture.customer.lastPropagation.RequestID != "request-http-1" ||
		fixture.customer.lastPropagation.Traceparent != request.Header.Get("traceparent") ||
		fixture.customer.lastPropagation.IdempotencyKey != "checkout-http-1" {
		t.Fatalf("propagation=%+v", fixture.customer.lastPropagation)
	}

	replayRequest := httptest.NewRequest(http.MethodPost, "/api/v1/checkouts",
		strings.NewReader(`{"cart_id":"CART-1","address_id":"ADDR-1"}`))
	replayRequest.Header = request.Header.Clone()
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, replayRequest)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
	}
}

func TestOrderHTTPProblemsAreStableAndListsUseOpaqueCursor(t *testing.T) {
	fixture := newCheckoutFixture()
	handler := NewHandler(fixture.service)

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/orders/ORD-X", nil))
	assertOrderProblem(t, missing, http.StatusNotFound, "order_not_found")

	badCursor := httptest.NewRecorder()
	handler.ServeHTTP(badCursor, httptest.NewRequest(http.MethodGet, "/api/v1/orders?cursor=not-base64", nil))
	assertOrderProblem(t, badCursor, http.StatusBadRequest, "invalid_cursor")

	badPage := httptest.NewRecorder()
	handler.ServeHTTP(badPage, httptest.NewRequest(http.MethodGet, "/api/v1/orders?page_size=0", nil))
	assertOrderProblem(t, badPage, http.StatusBadRequest, "invalid_page_size")
}

func postOrderJSON(handler http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	handler.ServeHTTP(response, request)
	return response
}

func assertOrderProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status=%d type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var problem httpx.Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != code || problem.Instance == "" {
		t.Fatalf("problem=%+v", problem)
	}
}
