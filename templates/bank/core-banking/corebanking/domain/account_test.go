package domain

import "testing"

func TestClose(t *testing.T) {
	got, err := Close(AccountStatusActive)
	if err != nil || got != AccountStatusClosed {
		t.Errorf("Close(active)=%q,%v, want closed", got, err)
	}
	if _, err := Close(AccountStatusFrozen); err == nil {
		t.Error("Close(frozen) should error")
	}
	if _, err := Close(AccountStatusClosed); err == nil {
		t.Error("Close(closed) should error (terminal state)")
	}
}

func TestFreezeUnfreeze(t *testing.T) {
	got, err := Freeze(AccountStatusActive)
	if err != nil || got != AccountStatusFrozen {
		t.Errorf("Freeze(active)=%q,%v, want frozen", got, err)
	}
	if _, err := Freeze(AccountStatusClosed); err == nil {
		t.Error("Freeze(closed) should error")
	}
	got, err = Unfreeze(AccountStatusFrozen)
	if err != nil || got != AccountStatusActive {
		t.Errorf("Unfreeze(frozen)=%q,%v, want active", got, err)
	}
	if _, err := Unfreeze(AccountStatusActive); err == nil {
		t.Error("Unfreeze(active) should error")
	}
}
