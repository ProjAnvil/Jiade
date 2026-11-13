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

func TestInventoryInvariantReserveUpdatesLevelAndReservationTogether(t *testing.T) {
	level := Level{OnHand: 10, Reserved: 2}
	reservation := Reservation{Quantity: 3, State: ReservationActive}

	gotLevel, gotReservation, err := level.Reserve(reservation)
	if err != nil {
		t.Fatal(err)
	}
	if gotLevel != (Level{OnHand: 10, Reserved: 5}) {
		t.Fatalf("level=%+v", gotLevel)
	}
	if gotReservation.State != ReservationActive {
		t.Fatalf("reservation state=%q", gotReservation.State)
	}
	if level.Reserved != 2 || reservation.State != ReservationActive {
		t.Fatal("reserve mutated its input values")
	}
}

func TestInventoryInvariantReserveRejectsInsufficientOrInvalidQuantity(t *testing.T) {
	tests := []struct {
		name        string
		level       Level
		reservation Reservation
	}{
		{name: "insufficient", level: Level{OnHand: 5, Reserved: 3}, reservation: Reservation{Quantity: 3, State: ReservationActive}},
		{name: "zero quantity", level: Level{OnHand: 5}, reservation: Reservation{State: ReservationActive}},
		{name: "negative quantity", level: Level{OnHand: 5}, reservation: Reservation{Quantity: -1, State: ReservationActive}},
		{name: "terminal reservation", level: Level{OnHand: 5}, reservation: Reservation{Quantity: 1, State: ReservationReleased}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotLevel, gotReservation, err := test.level.Reserve(test.reservation)
			if !errors.Is(err, ErrInvalidInventory) {
				t.Fatalf("error=%v, want ErrInvalidInventory", err)
			}
			if gotLevel != test.level || gotReservation != test.reservation {
				t.Fatalf("failed reserve changed values: level=%+v reservation=%+v", gotLevel, gotReservation)
			}
		})
	}
}

func TestInventoryInvariantReleaseAndExpiryReturnReservedStock(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(Level, Reservation) (Level, Reservation, error)
		want ReservationState
	}{
		{name: "release", run: Level.Release, want: ReservationReleased},
		{name: "expire", run: Level.Expire, want: ReservationExpired},
	} {
		t.Run(test.name, func(t *testing.T) {
			level := Level{OnHand: 10, Reserved: 5}
			reservation := Reservation{Quantity: 3, State: ReservationActive}
			gotLevel, gotReservation, err := test.run(level, reservation)
			if err != nil {
				t.Fatal(err)
			}
			if gotLevel != (Level{OnHand: 10, Reserved: 2}) || gotReservation.State != test.want {
				t.Fatalf("level=%+v reservation=%+v", gotLevel, gotReservation)
			}
		})
	}
}

func TestInventoryInvariantCommitConsumesOnHandAndReservedStock(t *testing.T) {
	level := Level{OnHand: 10, Reserved: 5}
	reservation := Reservation{Quantity: 3, State: ReservationActive}

	gotLevel, gotReservation, err := level.Commit(reservation)
	if err != nil {
		t.Fatal(err)
	}
	if gotLevel != (Level{OnHand: 7, Reserved: 2}) || gotReservation.State != ReservationCommitted {
		t.Fatalf("level=%+v reservation=%+v", gotLevel, gotReservation)
	}
}

func TestInventoryInvariantEffectsRejectUnderflowAndDuplicateTerminalEventsAreNoOps(t *testing.T) {
	level := Level{OnHand: 10, Reserved: 2}
	active := Reservation{Quantity: 3, State: ReservationActive}
	for _, run := range []func(Level, Reservation) (Level, Reservation, error){Level.Release, Level.Expire, Level.Commit} {
		gotLevel, gotReservation, err := run(level, active)
		if !errors.Is(err, ErrInvalidInventory) {
			t.Fatalf("underflow error=%v", err)
		}
		if gotLevel != level || gotReservation != active {
			t.Fatal("failed effect changed values")
		}
	}

	terminal := []struct {
		run         func(Level, Reservation) (Level, Reservation, error)
		reservation Reservation
	}{
		{run: Level.Release, reservation: Reservation{Quantity: 3, State: ReservationReleased}},
		{run: Level.Expire, reservation: Reservation{Quantity: 3, State: ReservationExpired}},
		{run: Level.Commit, reservation: Reservation{Quantity: 3, State: ReservationCommitted}},
	}
	for _, test := range terminal {
		gotLevel, gotReservation, err := test.run(level, test.reservation)
		if err != nil {
			t.Fatal(err)
		}
		if gotLevel != level || gotReservation != test.reservation {
			t.Fatal("duplicate terminal effect was not a no-op")
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
