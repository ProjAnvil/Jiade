package domain

import (
	"errors"
	"testing"
)

func TestHoldRelease_Active_To_Released(t *testing.T) {
	h := Hold{Status: HoldStatusActive, Amount: NewMoneyFromCents(10000)}
	if err := h.Release(); err != nil {
		t.Fatalf("active -> released 应成功: %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("状态=%q, want released", h.Status)
	}
}

func TestHoldRelease_Released_Idempotent(t *testing.T) {
	h := Hold{Status: HoldStatusReleased}
	if err := h.Release(); err != nil {
		t.Fatalf("released -> released 应幂等成功: %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("状态=%q, want released", h.Status)
	}
}

func TestHoldRelease_Captured_Rejected(t *testing.T) {
	h := Hold{Status: HoldStatusCaptured}
	err := h.Release()
	if !errors.Is(err, ErrHoldCaptured) {
		t.Fatalf("captured 释放应 ErrHoldCaptured, got %v", err)
	}
	if h.Status != HoldStatusCaptured {
		t.Errorf("状态被意外修改: %q", h.Status)
	}
}

func TestHoldCapture_Active_To_Captured(t *testing.T) {
	h := Hold{Status: HoldStatusActive}
	if err := h.Capture(); err != nil {
		t.Fatalf("active -> captured 应成功: %v", err)
	}
	if h.Status != HoldStatusCaptured {
		t.Errorf("状态=%q, want captured", h.Status)
	}
}

func TestHoldCapture_Captured_Idempotent(t *testing.T) {
	h := Hold{Status: HoldStatusCaptured}
	if err := h.Capture(); err != nil {
		t.Fatalf("captured -> captured 应幂等成功: %v", err)
	}
	if h.Status != HoldStatusCaptured {
		t.Errorf("状态=%q, want captured", h.Status)
	}
}

func TestHoldCapture_Released_Rejected(t *testing.T) {
	h := Hold{Status: HoldStatusReleased}
	err := h.Capture()
	if !errors.Is(err, ErrHoldReleased) {
		t.Fatalf("released 捕获应 ErrHoldReleased, got %v", err)
	}
	if h.Status != HoldStatusReleased {
		t.Errorf("状态被意外修改: %q", h.Status)
	}
}

func TestNewHoldID_UniqueAndPrefixed(t *testing.T) {
	id := NewHoldID()
	if len(id) < 8 || id[:1] != "H" {
		t.Errorf("NewHoldID 格式不对: %q", id)
	}
	id2 := NewHoldID()
	if id == id2 {
		t.Error("两次 NewHoldID 不应相同")
	}
}
