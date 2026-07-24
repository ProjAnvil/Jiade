// Package inventory contains pure domain rules for stock and reservations.
package inventory

import (
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidInventory  = errors.New("invalid inventory")
	ErrInvalidTransition = errors.New("invalid reservation transition")
)

// Level is an inventory snapshot for one SKU at one owned location.
type Level struct {
	OnHand   int64
	Reserved int64
}

// Available validates and returns stock that is not currently reserved.
func (level Level) Available() (int64, error) {
	if level.OnHand < 0 || level.Reserved < 0 || level.Reserved > level.OnHand {
		return 0, fmt.Errorf("%w: require on-hand >= reserved >= 0", ErrInvalidInventory)
	}
	return level.OnHand - level.Reserved, nil
}

// Reserve applies an active reservation to a level as one value operation.
// Persistence code can write both returned values in the same transaction.
func (level Level) Reserve(reservation Reservation) (Level, Reservation, error) {
	available, err := level.Available()
	if err != nil {
		return level, reservation, err
	}
	if reservation.Quantity <= 0 {
		return level, reservation, fmt.Errorf("%w: reservation quantity must be positive", ErrInvalidInventory)
	}
	if reservation.State != ReservationActive {
		return level, reservation, fmt.Errorf("%w: reservation must be active", ErrInvalidInventory)
	}
	if reservation.Quantity > available {
		return level, reservation, fmt.Errorf("%w: insufficient available stock", ErrInvalidInventory)
	}
	level.Reserved += reservation.Quantity
	return level, reservation, nil
}

// Release returns held stock without changing on-hand stock.
func (level Level) Release(reservation Reservation) (Level, Reservation, error) {
	return level.applyReservationEffect(reservation, ReservationRelease)
}

// Expire returns held stock and records an expiry.
func (level Level) Expire(reservation Reservation) (Level, Reservation, error) {
	return level.applyReservationEffect(reservation, ReservationExpire)
}

// Commit consumes both on-hand and reserved stock.
func (level Level) Commit(reservation Reservation) (Level, Reservation, error) {
	return level.applyReservationEffect(reservation, ReservationCommit)
}

type ReservationState string

const (
	ReservationActive    ReservationState = "active"
	ReservationCommitted ReservationState = "committed"
	ReservationReleased  ReservationState = "released"
	ReservationExpired   ReservationState = "expired"
)

type ReservationEvent string

const (
	ReservationCommit  ReservationEvent = "commit"
	ReservationRelease ReservationEvent = "release"
	ReservationExpire  ReservationEvent = "expire"
)

// Reservation records a conditional stock hold. Cross-service order and SKU
// identifiers are references rather than foreign keys.
type Reservation struct {
	ID             string
	OrderID        string
	SKU            string
	LocationID     string
	IdempotencyKey string
	Quantity       int64
	State          ReservationState
	ExpiresAt      time.Time
}

func NewReservation(id, orderID, sku, locationID, idempotencyKey string, quantity int64, expiresAt time.Time) (Reservation, error) {
	if id == "" || orderID == "" || sku == "" || locationID == "" || idempotencyKey == "" {
		return Reservation{}, fmt.Errorf("%w: reservation identifiers are required", ErrInvalidInventory)
	}
	if quantity <= 0 {
		return Reservation{}, fmt.Errorf("%w: reservation quantity must be positive", ErrInvalidInventory)
	}
	if expiresAt.IsZero() {
		return Reservation{}, fmt.Errorf("%w: reservation expiry is required", ErrInvalidInventory)
	}
	return Reservation{
		ID:             id,
		OrderID:        orderID,
		SKU:            sku,
		LocationID:     locationID,
		IdempotencyKey: idempotencyKey,
		Quantity:       quantity,
		State:          ReservationActive,
		ExpiresAt:      expiresAt,
	}, nil
}

// Transition returns a new reservation value. Duplicate events are no-ops;
// terminal reservations cannot move to a different state.
func (reservation Reservation) Transition(event ReservationEvent) (Reservation, error) {
	target, ok := reservationTarget(event)
	if !ok || !validReservationState(reservation.State) {
		return reservation, fmt.Errorf("%w: state %q event %q", ErrInvalidTransition, reservation.State, event)
	}
	if reservation.State == target {
		return reservation, nil
	}
	if reservation.State != ReservationActive {
		return reservation, fmt.Errorf("%w: state %q event %q", ErrInvalidTransition, reservation.State, event)
	}
	reservation.State = target
	return reservation, nil
}

func (level Level) applyReservationEffect(reservation Reservation, event ReservationEvent) (Level, Reservation, error) {
	if _, err := level.Available(); err != nil {
		return level, reservation, err
	}
	target, ok := reservationTarget(event)
	if !ok || !validReservationState(reservation.State) {
		return level, reservation, fmt.Errorf("%w: state %q event %q", ErrInvalidTransition, reservation.State, event)
	}
	if reservation.State == target {
		return level, reservation, nil
	}
	if reservation.State != ReservationActive {
		return level, reservation, fmt.Errorf("%w: state %q event %q", ErrInvalidTransition, reservation.State, event)
	}
	if reservation.Quantity <= 0 {
		return level, reservation, fmt.Errorf("%w: reservation quantity must be positive", ErrInvalidInventory)
	}
	if reservation.Quantity > level.Reserved {
		return level, reservation, fmt.Errorf("%w: reservation exceeds reserved stock", ErrInvalidInventory)
	}

	nextLevel := level
	nextLevel.Reserved -= reservation.Quantity
	if event == ReservationCommit {
		nextLevel.OnHand -= reservation.Quantity
	}
	nextReservation := reservation
	nextReservation.State = target
	if _, err := nextLevel.Available(); err != nil {
		return level, reservation, err
	}
	return nextLevel, nextReservation, nil
}

func reservationTarget(event ReservationEvent) (ReservationState, bool) {
	switch event {
	case ReservationCommit:
		return ReservationCommitted, true
	case ReservationRelease:
		return ReservationReleased, true
	case ReservationExpire:
		return ReservationExpired, true
	default:
		return "", false
	}
}

func validReservationState(state ReservationState) bool {
	switch state {
	case ReservationActive, ReservationCommitted, ReservationReleased, ReservationExpired:
		return true
	default:
		return false
	}
}
