package fulfillment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"commerce/internal/platform/httpx"
)

// FulfillmentOrderView is the JSON projection of a fulfillment order for the
// query API. It carries the per-location items, packages, and shipment
// projection so operators can inspect a delivery without joining raw tables.
type FulfillmentOrderView struct {
	FulfillmentID string                 `json:"fulfillment_id"`
	OrderID       string                 `json:"order_id"`
	LocationID    string                 `json:"location_id"`
	Status        FulfillmentOrderStatus `json:"status"`
	CreatedAt     string                 `json:"created_at,omitempty"`
	Items         []FulfillmentItem      `json:"items,omitempty"`
	Packages      []PackageDimension     `json:"packages,omitempty"`
	Shipment      *Shipment              `json:"shipment,omitempty"`
}

// NewHandler returns the fulfillment HTTP API: per-order lookup and per-
// fulfillment projection lookup. Writes are event-driven (see Consumer); the
// HTTP surface is read-only.
func NewHandler(store *PostgresStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/fulfillment/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		orderID := strings.TrimSpace(r.PathValue("id"))
		if orderID == "" {
			writeFulfillmentProblem(w, r, http.StatusBadRequest, "order_id_required")
			return
		}
		orders, err := store.GetFulfillmentsByOrder(r.Context(), orderID)
		if writeFulfillmentStoreError(w, r, err) {
			return
		}
		if len(orders) == 0 {
			writeFulfillmentProblem(w, r, http.StatusNotFound, "fulfillment_not_found")
			return
		}
		views := make([]FulfillmentOrderView, 0, len(orders))
		for _, order := range orders {
			views = append(views, buildFulfillmentOrderView(r.Context(), store, order))
		}
		writeFulfillmentJSON(w, http.StatusOK, map[string]any{
			"order_id": orderID, "fulfillments": views,
		})
	})
	mux.HandleFunc("GET /api/v1/fulfillment/{fulfillment_id}", func(w http.ResponseWriter, r *http.Request) {
		fulfillmentID := strings.TrimSpace(r.PathValue("fulfillment_id"))
		if fulfillmentID == "" {
			writeFulfillmentProblem(w, r, http.StatusBadRequest, "fulfillment_id_required")
			return
		}
		order, found, err := store.GetFulfillmentByID(r.Context(), fulfillmentID)
		if writeFulfillmentStoreError(w, r, err) {
			return
		}
		if !found {
			writeFulfillmentProblem(w, r, http.StatusNotFound, "fulfillment_not_found")
			return
		}
		writeFulfillmentJSON(w, http.StatusOK, buildFulfillmentOrderView(r.Context(), store, order))
	})
	return mux
}

func buildFulfillmentOrderView(ctx context.Context, store *PostgresStore, order FulfillmentOrder) FulfillmentOrderView {
	view := FulfillmentOrderView{
		FulfillmentID: order.FulfillmentID, OrderID: order.OrderID,
		LocationID: order.LocationID, Status: order.Status,
	}
	if !order.CreatedAt.IsZero() {
		view.CreatedAt = order.CreatedAt.UTC().Format(timeRFC3339Millis)
	}
	if items, err := store.ListItemsByFulfillment(ctx, order.FulfillmentID); err == nil {
		view.Items = items
	}
	if packages, err := store.ListPackagesByFulfillment(ctx, order.FulfillmentID); err == nil {
		view.Packages = packages
	}
	if shipment, err := store.GetShipmentByFulfillment(ctx, order.FulfillmentID); err == nil {
		view.Shipment = shipment
	}
	return view
}

const timeRFC3339Millis = "2006-01-02T15:04:05.000Z07:00"

func writeFulfillmentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeFulfillmentProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	httpx.WriteProblem(w, httpx.Problem{Status: status, Code: code, Instance: r.URL.Path})
}

func writeFulfillmentStoreError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFulfillmentNotFound) {
		writeFulfillmentProblem(w, r, http.StatusNotFound, "fulfillment_not_found")
		return true
	}
	writeFulfillmentProblem(w, r, http.StatusInternalServerError, "internal_error")
	return true
}
