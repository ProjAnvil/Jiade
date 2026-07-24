package fulfillment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	platformclient "commerce/internal/platform/client"
)

// InventoryHTTPClient fetches reservation allocations from the inventory
// service. It mirrors order's InventoryHTTPClient (internal/order/clients.go)
// but only the read side that fulfillment needs: GET /api/v1/reservations/{id}.
type InventoryHTTPClient struct {
	baseURL string
	client  *platformclient.Client
}

// NewInventoryHTTPClient binds a fulfillment-side inventory client to the
// resilient HTTP client shared with the rest of the service.
func NewInventoryHTTPClient(baseURL string, resilient *platformclient.Client) *InventoryHTTPClient {
	return &InventoryHTTPClient{baseURL: strings.TrimRight(baseURL, "/"), client: resilient}
}

// Ready probes the upstream readiness endpoint so process readiness waits for
// the inventory service before accepting traffic.
func (client *InventoryHTTPClient) Ready(ctx context.Context) error {
	if client == nil || client.client == nil || strings.TrimSpace(client.baseURL) == "" {
		return ErrUpstreamUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	response, err := client.client.Do(ctx, request, platformclient.Policy{})
	if err != nil {
		return fmt.Errorf("%w: inventory readiness: %v", ErrUpstreamUnavailable, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%w: inventory readiness status %d", ErrUpstreamUnavailable, response.StatusCode)
	}
	return nil
}

// ListReservations fetches the order's allocations from inventory. The upstream
// response is inventory's ReservationAllocation shape (sku, location_id,
// quantity, plus fields fulfillment does not consume); the lenient decoder
// keeps the read resilient to additive upstream schema changes.
func (client *InventoryHTTPClient) ListReservations(ctx context.Context, orderID string) (ReservationResult, error) {
	if client == nil || client.client == nil {
		return ReservationResult{}, fmt.Errorf("%w: inventory client is unavailable", ErrUpstreamUnavailable)
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return ReservationResult{}, ErrInvalidCommand
	}
	path := "/api/v1/reservations/" + url.PathEscape(orderID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+path, nil)
	if err != nil {
		return ReservationResult{}, fmt.Errorf("build inventory request: %w", err)
	}
	response, err := client.client.Do(ctx, request, platformclient.Policy{})
	if err != nil {
		return ReservationResult{}, fmt.Errorf("%w: inventory reservations: %v", ErrUpstreamUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		// No reservation yet — treat as an empty allocation set, not an error,
		// so the service can decide via ErrNoReservations.
		return ReservationResult{OrderID: orderID}, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ReservationResult{}, fmt.Errorf("%w: inventory status %d", ErrUpstreamUnavailable, response.StatusCode)
	}
	var body inventoryReservationResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return ReservationResult{}, fmt.Errorf("%w: decode inventory response: %v", ErrUpstreamUnavailable, err)
	}
	result := ReservationResult{OrderID: body.OrderID}
	if result.OrderID == "" {
		result.OrderID = orderID
	}
	result.Allocations = make(allocationList, 0, len(body.Allocations))
	for _, entry := range body.Allocations {
		if entry.SKU == "" || entry.LocationID == "" || entry.Quantity <= 0 {
			continue
		}
		result.Allocations = append(result.Allocations, Allocation{
			SKU: entry.SKU, LocationID: entry.LocationID, Quantity: entry.Quantity,
		})
	}
	return result, nil
}

// inventoryReservationResponse mirrors the inventory service's public response.
// The extra fields are preserved (not rejected) because inventory owns that
// contract and may add optional metadata without breaking fulfillment reads.
type inventoryReservationResponse struct {
	OrderID     string                        `json:"order_id"`
	Allocations []inventoryReservationEntry   `json:"allocations"`
}

type inventoryReservationEntry struct {
	SKU        string `json:"sku"`
	LocationID string `json:"location_id"`
	Quantity   int64  `json:"quantity"`
}

var _ InventoryReservationClient = (*InventoryHTTPClient)(nil)
