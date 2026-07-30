package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// HoldStatus funds-hold lifecycle status.
type HoldStatus string

const (
	HoldStatusActive   HoldStatus = "active"
	HoldStatusReleased HoldStatus = "released"
	HoldStatusCaptured HoldStatus = "captured"
)

// Hold state machine (legal transitions):
//
//	active  --Release--> released (final)
//	active  --Capture-->  captured (final)
//	released: Release is idempotent; Capture is rejected.
//	captured: Release and Capture are rejected.
var (
	// ErrHoldCaptured — a captured hold cannot be released.
	ErrHoldCaptured = errors.New("hold: 已捕获，不可释放")
	// ErrHoldReleased — a released hold cannot be captured.
	ErrHoldReleased = errors.New("hold: 已释放，不可捕获")
	// ErrInvalidHoldTransition — unknown/illegal status transition.
	ErrInvalidHoldTransition = errors.New("hold: 非法状态转换")
)

// Hold represents a reservation of available funds on a demand account,
// keyed by a unique idempotency key. The hold lifecycle is managed by
// Release/Capture; the ledger balance is only moved when a hold is captured
// (Task 3 held transfers) or released back to available.
type Hold struct {
	HoldID         string
	IdempotencyKey string
	AccountNo      string
	Amount         Money
	Ccy            string
	WorkflowID     string
	Status         HoldStatus
	ExpiresAt      time.Time // zero value = no expiry
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// PlaceHoldInput is the command to reserve available funds.
type PlaceHoldInput struct {
	IdempotencyKey string
	AccountNo      string
	Amount         Money
	Ccy            string
	WorkflowID     string
	ExpiresAt      time.Time // optional; zero = no expiry
}

// Release transitions active -> released. Calling Release on an already-released
// hold is idempotent (no-op, nil error). A captured hold is rejected with
// ErrHoldCaptured.
func (h *Hold) Release() error {
	switch h.Status {
	case HoldStatusActive:
		h.Status = HoldStatusReleased
		return nil
	case HoldStatusReleased:
		return nil
	case HoldStatusCaptured:
		return ErrHoldCaptured
	default:
		return fmt.Errorf("%w: %q", ErrInvalidHoldTransition, h.Status)
	}
}

// Capture transitions active -> captured. Calling Capture on an already-captured
// hold is idempotent (no-op, nil error). A released hold is rejected with
// ErrHoldReleased.
func (h *Hold) Capture() error {
	switch h.Status {
	case HoldStatusActive:
		h.Status = HoldStatusCaptured
		return nil
	case HoldStatusCaptured:
		return nil
	case HoldStatusReleased:
		return ErrHoldReleased
	default:
		return fmt.Errorf("%w: %q", ErrInvalidHoldTransition, h.Status)
	}
}

// NewHoldID generates a hold ID: "H" + 16 hex chars (crypto/rand).
func NewHoldID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "H" + hex.EncodeToString(b)
}
