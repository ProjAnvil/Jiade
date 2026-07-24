package customer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"commerce/internal/platform/httpx"
)

func TestCustomerListPaginationAndDetailPreservePublicPaths(t *testing.T) {
	store := &customerStoreStub{
		customers: []Customer{
			{ID: "CUS-1", Email: "one@example.test", Name: "One", Status: "active"},
			{ID: "CUS-2", Email: "two@example.test", Name: "Two", Status: "active"},
		},
		details: map[string]Customer{
			"CUS-1": {ID: "CUS-1", Email: "one@example.test", Name: "One", Status: "active",
				Addresses: []Address{{ID: "ADDR-1", CustomerID: "CUS-1", Recipient: "One", Phone: "13800000000", CountryCode: "CN", Province: "上海", City: "上海", District: "浦东", Line1: "世纪大道 1 号", PostalCode: "200000", Default: true}}},
		},
	}
	handler := NewHandler(NewService(store))
	list := httptest.NewRecorder()
	handler.ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/customers?page_size=1", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var page CustomerPage
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" {
		t.Fatalf("page=%+v", page)
	}
	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/customers/CUS-1", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"address_id":"ADDR-1"`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
}

func TestValidateAddressProvesOwnershipAndUsability(t *testing.T) {
	store := &customerStoreStub{validation: AddressValidation{
		CustomerID: "CUS-1", CustomerStatus: "active",
		Address: Address{ID: "ADDR-1", CustomerID: "CUS-1", Recipient: "One", Phone: "13800000000",
			CountryCode: "CN", Province: "上海", City: "上海", District: "浦东",
			Line1: "世纪大道 1 号", PostalCode: "200000", Default: true},
	}}
	handler := NewHandler(NewService(store))
	response := postCustomerJSON(handler, "/internal/v1/customer-addresses/validate",
		`{"customer_id":"CUS-1","address_id":"ADDR-1"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"valid":true`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	store.validation.Address.CustomerID = "CUS-OTHER"
	response = postCustomerJSON(handler, "/internal/v1/customer-addresses/validate",
		`{"customer_id":"CUS-1","address_id":"ADDR-1"}`)
	assertCustomerProblem(t, response, http.StatusUnprocessableEntity, "address_not_usable")

	store.validation.Address.CustomerID = "CUS-1"
	store.validation.CustomerStatus = "disabled"
	response = postCustomerJSON(handler, "/internal/v1/customer-addresses/validate",
		`{"customer_id":"CUS-1","address_id":"ADDR-1"}`)
	assertCustomerProblem(t, response, http.StatusUnprocessableEntity, "address_not_usable")
}

func TestValidateAddressRejectsMissingOrUnusableAddress(t *testing.T) {
	store := &customerStoreStub{validationErr: ErrAddressNotFound}
	handler := NewHandler(NewService(store))
	response := postCustomerJSON(handler, "/internal/v1/customer-addresses/validate",
		`{"customer_id":"CUS-1","address_id":"ADDR-X"}`)
	assertCustomerProblem(t, response, http.StatusNotFound, "address_not_found")

	store.validationErr = nil
	store.validation = AddressValidation{
		CustomerID: "CUS-1", CustomerStatus: "active",
		Address: Address{ID: "ADDR-1", CustomerID: "CUS-1", Recipient: "One"},
	}
	response = postCustomerJSON(handler, "/internal/v1/customer-addresses/validate",
		`{"customer_id":"CUS-1","address_id":"ADDR-1"}`)
	assertCustomerProblem(t, response, http.StatusUnprocessableEntity, "address_not_usable")
}

func TestCustomerJSONProblemsIncludeInstance(t *testing.T) {
	handler := NewHandler(NewService(&customerStoreStub{}))
	response := postCustomerJSON(handler, "/internal/v1/customer-addresses/validate", `{"customer_id":`)
	assertCustomerProblem(t, response, http.StatusBadRequest, "invalid_json")
}

func postCustomerJSON(handler http.Handler, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	return response
}

type customerStoreStub struct {
	customers     []Customer
	details       map[string]Customer
	validation    AddressValidation
	validationErr error
}

func (store *customerStoreStub) ListCustomers(_ context.Context, after string, limit int) ([]Customer, error) {
	start := 0
	for start < len(store.customers) && store.customers[start].ID <= after {
		start++
	}
	end := start + limit
	if end > len(store.customers) {
		end = len(store.customers)
	}
	return append([]Customer(nil), store.customers[start:end]...), nil
}

func (store *customerStoreStub) GetCustomer(_ context.Context, id string) (Customer, error) {
	customer, ok := store.details[id]
	if !ok {
		return Customer{}, ErrCustomerNotFound
	}
	return customer, nil
}

func (store *customerStoreStub) GetAddressValidation(context.Context, string, string) (AddressValidation, error) {
	return store.validation, store.validationErr
}

func assertCustomerProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
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

var _ Store = (*customerStoreStub)(nil)
