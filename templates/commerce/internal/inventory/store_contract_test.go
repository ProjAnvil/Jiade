package inventory

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresReservationStoreOwnsTransactionLockMovementAndOutboxSQL(t *testing.T) {
	source, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	required := []string{
		"pg_advisory_xact_lock",
		`lockOrder(ctx, tx, command.OrderID)`,
		`lockIdempotencyKey(ctx, tx, command.IdempotencyKey)`,
		`lockOrder(ctx, tx, orderID)`,
		"reservation_order_state",
		"FOR UPDATE OF i, l",
		"JOIN location_profile p",
		"SELECT p.location_id, p.fulfills_orders = true",
		"FOR UPDATE OF i, l, p",
		"FOR UPDATE OF r",
		"UPDATE inventory_level",
		"INSERT INTO reservation",
		"INSERT INTO stock_movement",
		"messaging.InsertOutbox",
		"tx.Commit(ctx)",
	}
	for _, fragment := range required {
		if !strings.Contains(text, fragment) {
			t.Errorf("store.go missing transactional gate %q", fragment)
		}
	}
}

func TestTerminalFenceWaitsForActiveReservationExpiry(t *testing.T) {
	now := fixedInventoryClock()
	active := []ReservationAllocation{{
		ID: "RES-1", OrderID: "ORD-1", SKU: "SKU-1", LocationID: "LOC-1",
		Quantity: 1, State: ReservationActive, ExpiresAt: now.Add(time.Minute),
	}}
	if canApplyTerminalFence(active, ReservationExpire, now) {
		t.Fatal("expiry fence applied before active reservation expiry")
	}
	if !canApplyTerminalFence(active, ReservationRelease, now) {
		t.Fatal("release fence unexpectedly delayed")
	}
	if !canApplyTerminalFence(nil, ReservationExpire, now) {
		t.Fatal("terminal-before-reserve expiry did not create a fence")
	}
	active[0].ExpiresAt = now
	if !canApplyTerminalFence(active, ReservationExpire, now) {
		t.Fatal("due expiry fence was delayed")
	}
}

func TestPostgresReservationStoreDoesNotEnableMissingProfiles(t *testing.T) {
	source, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, forbidden := range []string{
		"COALESCE(p.fulfills_orders, true)",
		"LEFT JOIN location_profile",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("store.go retains permissive profile fallback %q", forbidden)
		}
	}
}

func TestHandlersContainNoSQL(t *testing.T) {
	source, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(string(source))
	for _, keyword := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE "} {
		if strings.Contains(upper, keyword) {
			t.Errorf("inventory handler contains SQL keyword %q", keyword)
		}
	}
}
