package order

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	platformclient "commerce/internal/platform/client"
)

type CustomerHTTPClient struct {
	baseURL string
	client  *platformclient.Client
}

func NewCustomerHTTPClient(baseURL string, resilient *platformclient.Client) *CustomerHTTPClient {
	return &CustomerHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), client: resilient}
}

func (client *CustomerHTTPClient) Validate(
	ctx context.Context,
	customerID string,
	addressID string,
	propagation Propagation,
) (CustomerSnapshot, error) {
	if client == nil || client.client == nil {
		return CustomerSnapshot{}, fmt.Errorf("%w: customer client is unavailable", ErrUpstreamUnavailable)
	}
	var validation struct {
		Valid    bool            `json:"valid"`
		Customer json.RawMessage `json:"customer"`
		Address  json.RawMessage `json:"address"`
	}
	if err := client.doJSON(ctx, http.MethodPost, "/internal/v1/customer-addresses/validate",
		map[string]string{"customer_id": customerID, "address_id": addressID}, propagation, &validation); err != nil {
		return CustomerSnapshot{}, err
	}
	if !validation.Valid || len(validation.Address) == 0 {
		return CustomerSnapshot{}, ErrInvalidCommand
	}
	var customer struct {
		ID    string `json:"customer_id"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Phone string `json:"phone"`
	}
	if err := client.doJSON(ctx, http.MethodGet, "/api/v1/customers/"+url.PathEscape(customerID),
		nil, propagation, &customer); err != nil {
		return CustomerSnapshot{}, err
	}
	if customer.ID != customerID || customer.Email == "" || customer.Name == "" {
		return CustomerSnapshot{}, ErrInvalidCommand
	}
	return CustomerSnapshot{
		ID: customer.ID, Email: customer.Email, Name: customer.Name, Phone: customer.Phone,
		Address: append(json.RawMessage(nil), validation.Address...),
	}, nil
}

func (client *CustomerHTTPClient) doJSON(
	ctx context.Context,
	method string,
	path string,
	body any,
	propagation Propagation,
	destination any,
) error {
	return doDependencyJSON(ctx, client.client, client.baseURL, method, path, body, propagation, destination)
}

type CatalogHTTPClient struct {
	baseURL string
	client  *platformclient.Client
}

func NewCatalogHTTPClient(baseURL string, resilient *platformclient.Client) *CatalogHTTPClient {
	return &CatalogHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), client: resilient}
}

func (client *CatalogHTTPClient) Snapshot(
	ctx context.Context,
	skus []string,
	propagation Propagation,
) ([]CatalogSnapshot, error) {
	if client == nil || client.client == nil {
		return nil, fmt.Errorf("%w: catalog client is unavailable", ErrUpstreamUnavailable)
	}
	snapshots := make([]CatalogSnapshot, 0, len(skus))
	for _, sku := range skus {
		var response catalogSnapshotResponse
		path := "/internal/v1/catalog/skus/" + url.PathEscape(sku)
		_, err := doDependencyJSONStatus(ctx, client.client, client.baseURL, http.MethodGet,
			path, nil, propagation, &response)
		if err != nil {
			return nil, err
		}
		snapshot, err := response.snapshotFor(sku)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

type catalogSnapshotResponse struct {
	SKU              string `json:"sku"`
	Title            string `json:"title"`
	PriceMinor       int64  `json:"price_minor"`
	UnitPriceMinor   int64  `json:"unit_price_minor"`
	Currency         string `json:"currency"`
	ProductTitle     string `json:"product_title"`
	Status           string `json:"status"`
	AvailableForSale bool   `json:"available_for_sale"`
}

func (response catalogSnapshotResponse) snapshotFor(sku string) (CatalogSnapshot, error) {
	if response.SKU == sku {
		if response.Status != "active" || !response.AvailableForSale {
			return CatalogSnapshot{}, fmt.Errorf("%w: catalog SKU %s is not saleable", ErrInvalidCommand, sku)
		}
		price := response.UnitPriceMinor
		if price == 0 {
			price = response.PriceMinor
		}
		return CatalogSnapshot{
			SKU: sku, Title: response.Title, UnitPriceMinor: price, Currency: response.Currency,
		}, nil
	}
	return CatalogSnapshot{}, fmt.Errorf("%w: catalog did not return SKU %s", ErrInvalidCommand, sku)
}

type InventoryHTTPClient struct {
	baseURL string
	client  *platformclient.Client
}

func NewInventoryHTTPClient(baseURL string, resilient *platformclient.Client) *InventoryHTTPClient {
	return &InventoryHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), client: resilient}
}

func (client *InventoryHTTPClient) Reserve(
	ctx context.Context,
	command ReservationCommand,
	propagation Propagation,
) error {
	if client == nil || client.client == nil {
		return fmt.Errorf("%w: inventory client is unavailable", ErrUpstreamUnavailable)
	}
	propagation.IdempotencyKey = command.IdempotencyKey
	var result json.RawMessage
	return doDependencyJSON(ctx, client.client, client.baseURL, http.MethodPost,
		"/internal/v1/reservations", command, propagation, &result)
}

func (client *InventoryHTTPClient) Release(ctx context.Context, orderID string, propagation Propagation) error {
	if client == nil || client.client == nil {
		return fmt.Errorf("%w: inventory client is unavailable", ErrUpstreamUnavailable)
	}
	var result json.RawMessage
	return doDependencyJSON(ctx, client.client, client.baseURL, http.MethodPost,
		"/internal/v1/reservations/"+url.PathEscape(orderID)+"/release",
		struct{}{}, propagation, &result)
}

func doDependencyJSON(
	ctx context.Context,
	resilient *platformclient.Client,
	baseURL string,
	method string,
	path string,
	body any,
	propagation Propagation,
	destination any,
) error {
	_, err := doDependencyJSONStatus(ctx, resilient, baseURL, method, path, body, propagation, destination)
	return err
}

func doDependencyJSONStatus(
	ctx context.Context,
	resilient *platformclient.Client,
	baseURL string,
	method string,
	path string,
	body any,
	propagation Propagation,
	destination any,
) (int, error) {
	if resilient == nil || strings.TrimSpace(baseURL) == "" {
		return 0, fmt.Errorf("%w: dependency client is not configured", ErrUpstreamUnavailable)
	}
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode dependency request: %w", err)
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return 0, fmt.Errorf("build dependency request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	applyPropagation(request.Header, propagation)
	response, err := resilient.Do(ctx, request, platformclient.Policy{})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, dependencyStatusError(response)
	}
	if destination == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return response.StatusCode, err
	}
	if raw, ok := destination.(*json.RawMessage); ok {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return response.StatusCode, fmt.Errorf("read dependency response: %w", err)
		}
		*raw = append((*raw)[:0], body...)
		return response.StatusCode, nil
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(destination); err != nil {
		return response.StatusCode, fmt.Errorf("%w: decode dependency response: %v", ErrUpstreamUnavailable, err)
	}
	return response.StatusCode, nil
}

func applyPropagation(header http.Header, propagation Propagation) {
	if propagation.RequestID != "" {
		header.Set("X-Request-ID", propagation.RequestID)
	}
	if propagation.Traceparent != "" {
		header.Set("traceparent", propagation.Traceparent)
	}
	if propagation.IdempotencyKey != "" {
		header.Set("Idempotency-Key", propagation.IdempotencyKey)
	}
	if propagation.CorrelationID != "" {
		header.Set("X-Correlation-ID", propagation.CorrelationID)
	}
	if propagation.CausationID != "" {
		header.Set("X-Causation-ID", propagation.CausationID)
	}
}

func dependencyStatusError(response *http.Response) error {
	var problem struct {
		Code string `json:"code"`
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = json.Unmarshal(body, &problem)
	switch {
	case response.StatusCode == http.StatusConflict && problem.Code == "idempotency_conflict":
		return ErrIdempotencyConflict
	case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnprocessableEntity ||
		response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusConflict:
		return fmt.Errorf("%w: dependency status %d code %s", ErrInvalidCommand, response.StatusCode, problem.Code)
	default:
		return fmt.Errorf("%w: dependency status %d code %s", ErrUpstreamUnavailable, response.StatusCode, problem.Code)
	}
}

var (
	_ CustomerClient  = (*CustomerHTTPClient)(nil)
	_ CatalogClient   = (*CatalogHTTPClient)(nil)
	_ InventoryClient = (*InventoryHTTPClient)(nil)
)
