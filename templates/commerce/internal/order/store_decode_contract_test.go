package order

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"commerce/internal/inventory"
	"commerce/internal/platform/messaging"
)

// TestDecodeInventoryCommittedEventMatchesInventoryShape is the cross-service
// contract test for the inventory saga happy path. It builds the
// inventory.committed.v1 event payload from inventory's actual
// ReservationAllocation shape (the 7-field struct inventory emits from
// internal/inventory/store.go insertInventoryEvent) and asserts that order's
// DisallowUnknownFields decoder in decodeOrderResult accepts it.
//
// This test guards the Task 7a regression class: a producer-side field added
// without a matching consumer-side field makes every real event fail to decode
// into messaging.NonRetryable -> DLQ, silently stalling the saga.
func TestDecodeInventoryCommittedEventMatchesInventoryShape(t *testing.T) {
	const orderID = "ORD-CONTRACT-1"
	expiresAt := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// Build the allocation with inventory's real 7-field struct, exactly as
	// inventory's store.go emits it in insertInventoryEvent -> json.Marshal(
	// ReservationResult{...}). Any field added on the producer side without a
	// matching tag here surfaces as a missing tag rather than a silent skip.
	allocation := inventory.ReservationAllocation{
		ID:         "RES-CONTRACT-1",
		OrderID:    orderID,
		SKU:        "SKU-CONTRACT-1",
		LocationID: "LOC-CONTRACT-1",
		Quantity:   3,
		State:      inventory.ReservationCommitted,
		ExpiresAt:  expiresAt,
	}
	body, err := json.Marshal(inventory.ReservationResult{
		OrderID:     orderID,
		Allocations: []inventory.ReservationAllocation{allocation},
	})
	if err != nil {
		t.Fatalf("marshal inventory payload: %v", err)
	}

	event := messaging.NewEvent("inventory.committed.v1", orderID, "contract", "",
		body, func() time.Time { return expiresAt })

	var payload inventoryResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		t.Fatalf("order failed to decode real inventory.committed.v1 payload: %v\n"+
			"payload=%s", err, string(body))
	}
	if payload.OrderID != orderID || len(payload.Allocations) != 1 {
		t.Fatalf("decoded payload mismatch: order_id=%q allocations=%d",
			payload.OrderID, len(payload.Allocations))
	}
	got := payload.Allocations[0]
	if got.AllocationID != allocation.ID || got.SKU != allocation.SKU ||
		got.Quantity != allocation.Quantity || got.Status != string(allocation.State) {
		t.Fatalf("decoded allocation mismatch: got %+v want %+v", got, allocation)
	}
}

// TestDecodeInventoryReleasedEventMatchesInventoryShape is the same contract
// for the compensation path so an inventory.released.v1 carrying the 7-field
// allocation also decodes cleanly.
func TestDecodeInventoryReleasedEventMatchesInventoryShape(t *testing.T) {
	const orderID = "ORD-CONTRACT-2"
	occurredAt := time.Date(2026, 7, 24, 12, 30, 0, 0, time.UTC)

	allocation := inventory.ReservationAllocation{
		ID:         "RES-CONTRACT-2",
		OrderID:    orderID,
		SKU:        "SKU-CONTRACT-2",
		LocationID: "LOC-CONTRACT-2",
		Quantity:   2,
		State:      inventory.ReservationReleased,
		ExpiresAt:  occurredAt,
	}
	body, err := json.Marshal(inventory.ReservationResult{
		OrderID:     orderID,
		Allocations: []inventory.ReservationAllocation{allocation},
	})
	if err != nil {
		t.Fatalf("marshal inventory payload: %v", err)
	}

	event := messaging.NewEvent("inventory.released.v1", orderID, "contract", "",
		body, func() time.Time { return occurredAt })

	var payload inventoryResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		t.Fatalf("order failed to decode real inventory.released.v1 payload: %v\n"+
			"payload=%s", err, string(body))
	}
	if payload.OrderID != orderID || len(payload.Allocations) != 1 {
		t.Fatalf("decoded payload mismatch: order_id=%q allocations=%d",
			payload.OrderID, len(payload.Allocations))
	}
	if !errors.Is(err, nil) {
		t.Fatalf("unexpected error: %v", err)
	}
	got := payload.Allocations[0]
	if got.AllocationID != allocation.ID || got.SKU != allocation.SKU ||
		got.Quantity != allocation.Quantity || got.Status != string(allocation.State) {
		t.Fatalf("decoded allocation mismatch: got %+v want %+v", got, allocation)
	}
}
