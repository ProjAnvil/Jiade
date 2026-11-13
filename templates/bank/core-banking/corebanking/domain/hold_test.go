package domain

import (
	"errors"
	"testing"
)

func TestHoldRelease_Active_To_Released(t *testing.T) {
	h := Hold{Status: HoldStatusActive, Amount: NewMoneyFromCents(10000)}
	if err := h.Release(); err != nil {
		t.Fatalf("active -> released should succeed: %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("status=%q, want released", h.Status)
	}
}

func TestHoldRelease_Released_Idempotent(t *testing.T) {
	h := Hold{Status: HoldStatusReleased}
	if err := h.Release(); err != nil {
		t.Fatalf("released -> released should succeed idempotently: %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("status=%q, want released", h.Status)
	}
}

func TestHoldRelease_Captured_Rejected(t *testing.T) {
	h := Hold{Status: HoldStatusCaptured}
	err := h.Release()
	if !errors.Is(err, ErrHoldCaptured) {
		t.Fatalf("releasing captured should give ErrHoldCaptured, got %v", err)
	}
	if h.Status != HoldStatusCaptured {
		t.Errorf("status unexpectedly modified: %q", h.Status)
	}
}

func TestHoldCapture_Active_To_Captured(t *testing.T) {
	h := Hold{Status: HoldStatusActive}
	if err := h.Capture(); err != nil {
		t.Fatalf("active -> captured should succeed: %v", err)
	}
	if h.Status != HoldStatusCaptured {
		t.Errorf("status=%q, want captured", h.Status)
	}
}

func TestHoldCapture_Captured_Idempotent(t *testing.T) {
	h := Hold{Status: HoldStatusCaptured}
	if err := h.Capture(); err != nil {
		t.Fatalf("captured -> captured should succeed idempotently: %v", err)
	}
	if h.Status != HoldStatusCaptured {
		t.Errorf("status=%q, want captured", h.Status)
	}
}

func TestHoldCapture_Released_Rejected(t *testing.T) {
	h := Hold{Status: HoldStatusReleased}
	err := h.Capture()
	if !errors.Is(err, ErrHoldReleased) {
		t.Fatalf("capturing released should give ErrHoldReleased, got %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("status unexpectedly modified: %q", h.Status)
	}
}

func TestNewHoldID_UniqueAndPrefixed(t *testing.T) {
	id := NewHoldID()
	if len(id) < 8 || id[:1] != "H" {
		t.Errorf("NewHoldID wrong format: %q", id)
	}
	id2 := NewHoldID()
	if id == id2 {
		t.Error("two NewHoldID calls should differ")
	}
}
