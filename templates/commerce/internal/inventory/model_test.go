package inventory

import (
	"errors"
	"testing"
	"time"
)

func TestInventoryInvariantLevelAvailable(t *testing.T) {
	level := Level{OnHand: 12, Reserved: 5}
	got, err := level.Available()
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("available=%d, want 7", got)
	}
}

func TestInventoryInvariantLevelRejectsInvalidQuantities(t *testing.T) {
	for _, level := range []Level{
		{OnHand: -1, Reserved: 0},
		{OnHand: 1, Reserved: -1},
		{OnHand: 1, Reserved: 2},
	} {
		if _, err := level.Available(); !errors.Is(err, ErrInvalidInventory) {
			t.Fatalf("level=%+v error=%v, want ErrInvalidInventory", level, err)
		}
	}
}

func TestInventoryInvariantNewReservationRequiresValidFields(t *testing.T) {
	expiry := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	got, err := NewReservation("RES-1", "ORD-1", "SKU-1", "LOC-1", "reserve-1", 3, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ReservationActive || got.Quantity != 3 || !got.ExpiresAt.Equal(expiry) {
		t.Fatalf("reservation=%+v", got)
	}

	tests := []struct {
		name                              string
		id, orderID, sku, locationID, key string
		quantity                          int64
		expiresAt                         time.Time
	}{
		{name: "missing ID", orderID: "ORD-1", sku: "SKU-1", locationID: "LOC-1", key: "key", quantity: 1, expiresAt: expiry},
		{name: "missing order", id: "RES-1", sku: "SKU-1", locationID: "LOC-1", key: "key", quantity: 1, expiresAt: expiry},
		{name: "missing SKU", id: "RES-1", orderID: "ORD-1", locationID: "LOC-1", key: "key", quantity: 1, expiresAt: expiry},
		{name: "missing location", id: "RES-1", orderID: "ORD-1", sku: "SKU-1", key: "key", quantity: 1, expiresAt: expiry},
		{name: "missing key", id: "RES-1", orderID: "ORD-1", sku: "SKU-1", locationID: "LOC-1", quantity: 1, expiresAt: expiry},
		{name: "zero quantity", id: "RES-1", orderID: "ORD-1", sku: "SKU-1", locationID: "LOC-1", key: "key", expiresAt: expiry},
		{name: "missing expiry", id: "RES-1", orderID: "ORD-1", sku: "SKU-1", locationID: "LOC-1", key: "key", quantity: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewReservation(test.id, test.orderID, test.sku, test.locationID, test.key, test.quantity, test.expiresAt)
			if !errors.Is(err, ErrInvalidInventory) {
				t.Fatalf("error=%v, want ErrInvalidInventory", err)
			}
		})
	}
}

func TestInventoryInvariantReservationTransitionsAreConditionalAndIdempotent(t *testing.T) {
	tests := []struct {
		event ReservationEvent
		want  ReservationState
	}{
		{event: ReservationCommit, want: ReservationCommitted},
		{event: ReservationRelease, want: ReservationReleased},
		{event: ReservationExpire, want: ReservationExpired},
	}
	for _, test := range tests {
		reservation := Reservation{State: ReservationActive}
		got, err := reservation.Transition(test.event)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != test.want {
			t.Fatalf("state=%q, want %q", got.State, test.want)
		}
		duplicate, err := got.Transition(test.event)
		if err != nil {
			t.Fatalf("duplicate transition: %v", err)
		}
		if duplicate.State != test.want {
			t.Fatalf("duplicate state=%q, want %q", duplicate.State, test.want)
		}
	}
}

func TestInventoryInvariantReservationTerminalStatesCannotMove(t *testing.T) {
	for _, state := range []ReservationState{ReservationCommitted, ReservationReleased, ReservationExpired} {
		for _, event := range []ReservationEvent{ReservationCommit, ReservationRelease, ReservationExpire} {
			if targetState(event) == state {
				continue
			}
			_, err := (Reservation{State: state}).Transition(event)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("state=%q event=%q error=%v", state, event, err)
			}
		}
	}
}

func targetState(event ReservationEvent) ReservationState {
	switch event {
	case ReservationCommit:
		return ReservationCommitted
	case ReservationRelease:
		return ReservationReleased
	case ReservationExpire:
		return ReservationExpired
	default:
		return ""
	}
}
