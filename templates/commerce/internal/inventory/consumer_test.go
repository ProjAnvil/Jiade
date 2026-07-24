package inventory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
)

// TestConsumerApplyCommitRequestedTransitionsActiveReservations verifies the
// inventory saga consumer closes the commit loop: an inventory.commit-requested.v1
// event drives the active reservation to committed through the service, exactly
// as payment capture expects before order can reach paid/confirmed.
func TestConsumerApplyCommitRequestedTransitionsActiveReservations(t *testing.T) {
	store := newInventoryStoreStub()
	// Seed an active reservation the way order's checkout would have created it.
	if _, err := store.Reserve(nil, ReserveCommand{
		OrderID: "ORD-COMMIT-1", IdempotencyKey: "reserve-1",
		Lines:      []ReserveLine{{SKU: "SKU-1", Quantity: 2}},
		OccurredAt: fixedInventoryClock(), ExpiresAt: fixedInventoryClock().Add(time.Minute),
	}); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	service := NewService(store, fixedInventoryClock)
	consumer := &Consumer{store: &PostgresStore{}, service: service, policy: messaging.RetryPolicy{}}

	body, _ := json.Marshal(map[string]any{"order_id": "ORD-COMMIT-1"})
	event := messaging.NewEvent("inventory.commit-requested.v1", "ORD-COMMIT-1", "corr-1", "",
		body, func() time.Time { return fixedInventoryClock() })

	if err := consumer.applyEvent(event); err != nil {
		t.Fatalf("applyCommitRequested error: %v", err)
	}
	allocations, err := store.ListReservationsByOrder(nil, "ORD-COMMIT-1")
	if err != nil {
		t.Fatalf("ListReservationsByOrder: %v", err)
	}
	if len(allocations) != 1 || allocations[0].State != ReservationCommitted {
		t.Fatalf("allocation state=%v, want committed", allocations)
	}
	if store.transitionChanges != 1 {
		t.Fatalf("transitionChanges=%d, want 1", store.transitionChanges)
	}
}

// TestConsumerApplyReleaseRequestedIsIdempotentAcrossReplays verifies that a
// duplicate release-requested event is a no-op (no error, no extra transition)
// once the order is already terminal-released. This is the convergence point
// between the order HTTP Release fast-path and the event-driven consumer: both
// paths may fire and the second must not error.
func TestConsumerApplyReleaseRequestedIsIdempotentAcrossReplays(t *testing.T) {
	store := newInventoryStoreStub()
	if _, err := store.Reserve(nil, ReserveCommand{
		OrderID: "ORD-RELEASE-1", IdempotencyKey: "reserve-2",
		Lines:      []ReserveLine{{SKU: "SKU-2", Quantity: 1}},
		OccurredAt: fixedInventoryClock(), ExpiresAt: fixedInventoryClock().Add(time.Minute),
	}); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	service := NewService(store, fixedInventoryClock)
	consumer := &Consumer{store: &PostgresStore{}, service: service, policy: messaging.RetryPolicy{}}

	body, _ := json.Marshal(map[string]any{"order_id": "ORD-RELEASE-1"})
	event := messaging.NewEvent("inventory.release-requested.v1", "ORD-RELEASE-1", "corr-2", "",
		body, func() time.Time { return fixedInventoryClock() })

	if err := consumer.applyEvent(event); err != nil {
		t.Fatalf("first applyReleaseRequested error: %v", err)
	}
	changesAfterFirst := store.transitionChanges

	// Replay: the order is already terminal=release; TransitionOrder returns
	// (allocations, false, nil) and the consumer must not surface an error.
	if err := consumer.applyEvent(event); err != nil {
		t.Fatalf("replay applyReleaseRequested error: %v", err)
	}
	if store.transitionChanges != changesAfterFirst {
		t.Fatalf("replay transitionChanges=%d, want %d (release must be idempotent)",
			store.transitionChanges, changesAfterFirst)
	}
	allocations, _ := store.ListReservationsByOrder(nil, "ORD-RELEASE-1")
	if len(allocations) != 1 || allocations[0].State != ReservationReleased {
		t.Fatalf("allocation state=%v, want released", allocations)
	}
}

// TestConsumerRejectsPayloadSubjectMismatch guards the strict subject/order_id
// equality check so a malformed or mismatched event is sent to the DLQ rather
// than transitioning the wrong reservation.
func TestConsumerRejectsPayloadSubjectMismatch(t *testing.T) {
	store := newInventoryStoreStub()
	service := NewService(store, fixedInventoryClock)
	consumer := &Consumer{store: &PostgresStore{}, service: service, policy: messaging.RetryPolicy{}}

	body, _ := json.Marshal(map[string]any{"order_id": "ORD-OTHER"})
	event := messaging.NewEvent("inventory.commit-requested.v1", "ORD-SUBJECT", "corr", "",
		body, func() time.Time { return fixedInventoryClock() })

	err := consumer.applyEvent(event)
	if err == nil || !strings.Contains(err.Error(), "does not match subject") {
		t.Fatalf("error=%v, want subject mismatch", err)
	}
	if store.transitionChanges != 0 {
		t.Fatalf("transitionChanges=%d, want 0 (mismatched event must not transition)",
			store.transitionChanges)
	}
}

// TestConsumerRejectsUnknownEventType ensures unsupported events go to the DLQ.
func TestConsumerRejectsUnknownEventType(t *testing.T) {
	store := newInventoryStoreStub()
	service := NewService(store, fixedInventoryClock)
	consumer := &Consumer{store: &PostgresStore{}, service: service, policy: messaging.RetryPolicy{}}

	body, _ := json.Marshal(map[string]any{"order_id": "ORD-1"})
	event := messaging.NewEvent("inventory.unknown.v1", "ORD-1", "corr", "",
		body, func() time.Time { return fixedInventoryClock() })

	err := consumer.applyEvent(event)
	if err == nil || !strings.Contains(err.Error(), "unsupported inventory event type") {
		t.Fatalf("error=%v, want unsupported inventory event type", err)
	}
}
