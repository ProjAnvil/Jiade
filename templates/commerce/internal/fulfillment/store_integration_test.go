//go:build integration

package fulfillment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresFulfillOrderSplitsByLocationAtomically(t *testing.T) {
	store, pool := newIntegrationFulfillmentStore(t)
	ctx := context.Background()
	now := integrationFulfillmentClock()
	outcome := buildIntegrationOutcome("ORD-INT-1", "paid-int-1", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 2},
		{SKU: "SKU-B", LocationID: "LOC-2", Quantity: 1},
		{SKU: "SKU-C", LocationID: "LOC-1", Quantity: 4},
	}, now, "corr-int-1", "")
	result, err := store.SaveFulfill(ctx, outcome)
	if err != nil {
		t.Fatalf("SaveFulfill error: %v", err)
	}
	if len(result.FulfillmentOrders) != 2 {
		t.Fatalf("fulfillment orders=%d, want 2 (one per location)", len(result.FulfillmentOrders))
	}
	if result.Replayed {
		t.Fatalf("first fulfillment must not be a replay")
	}
	assertFulfillmentCount(t, pool, "fulfillment_order", 2)
	assertFulfillmentCount(t, pool, "fulfillment_item", 3)
	assertFulfillmentCount(t, pool, "pick_item", 3)
	assertFulfillmentCount(t, pool, "package", 2)
	assertFulfillmentCount(t, pool, "shipment", 2)
	assertFulfillmentCount(t, pool, "tracking_event", 6) // 3 events x 2 shipments
	assertFulfillmentCount(t, pool, "outbox_event", 1)

	// Replay must be idempotent: ON CONFLICT DO NOTHING on (order_id,
	// location_id) short-circuits and no new outbox row is inserted.
	replay, err := store.SaveFulfill(ctx, outcome)
	if err != nil {
		t.Fatalf("replay SaveFulfill error: %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("replay SaveFulfill must return Replayed=true")
	}
	assertFulfillmentCount(t, pool, "fulfillment_order", 2)
	assertFulfillmentCount(t, pool, "outbox_event", 1)
}

func TestPostgresCancelFlipsOpenOrdersAndEmitsEvent(t *testing.T) {
	store, pool := newIntegrationFulfillmentStore(t)
	ctx := context.Background()
	now := integrationFulfillmentClock()
	// Seed two OPEN fulfillment orders directly (no shipment) so the cancel
	// path can flip their status to 'cancelled'.
	for _, location := range []string{"LOC-1", "LOC-2"} {
		order := FulfillmentOrder{
			FulfillmentID: deterministicFulfillmentID("ORD-INT-2", location),
			OrderID:       "ORD-INT-2", LocationID: location,
			Status: StatusOpen, CreatedAt: now,
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO fulfillment_order (
				fulfillment_id, order_id, location_id, status, created_at
			) VALUES ($1, $2, $3, $4, $5)`,
			order.FulfillmentID, order.OrderID, order.LocationID,
			string(order.Status), order.CreatedAt); err != nil {
			t.Fatalf("seed order %s: %v", location, err)
		}
	}
	cancelledEvent := messaging.NewEvent(EventFulfillmentCancelled, "ORD-INT-2", "corr-int-2", "",
		mustFulfillmentJSON(orderIDPayload{OrderID: "ORD-INT-2"}),
		func() time.Time { return now })
	result, err := store.SaveCancel(ctx, CancelOutcome{
		IdempotencyKey: "cancel-int-2", OrderID: "ORD-INT-2",
		Reason: "buyer_cancelled", Events: []messaging.Event{cancelledEvent},
	})
	if err != nil {
		t.Fatalf("SaveCancel error: %v", err)
	}
	if len(result.FulfillmentOrders) != 2 {
		t.Fatalf("cancelled orders=%d, want 2", len(result.FulfillmentOrders))
	}
	for _, order := range result.FulfillmentOrders {
		if order.Status != StatusCancelled {
			t.Fatalf("order %s status=%q, want cancelled",
				order.FulfillmentID, order.Status)
		}
	}
	assertFulfillmentCount(t, pool, "outbox_event", 1)

	// Replay with the same idempotency key finds no open orders and must NOT
	// insert a duplicate outbox row (Replayed=true, no events).
	replay, err := store.SaveCancel(ctx, CancelOutcome{
		IdempotencyKey: "cancel-int-2", OrderID: "ORD-INT-2",
		Reason: "buyer_cancelled", Events: []messaging.Event{cancelledEvent},
	})
	if err != nil {
		t.Fatalf("replay SaveCancel error: %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("replay SaveCancel must return Replayed=true")
	}
	if len(replay.Events) != 0 {
		t.Fatalf("replay events=%d, want 0 (no duplicates)", len(replay.Events))
	}
	assertFulfillmentCount(t, pool, "outbox_event", 1)
}

func TestPostgresCancelEmitsEventEvenWhenAlreadyFulfilled(t *testing.T) {
	// Regression test for the saga-hang bug: when every fulfillment_order for
	// the order is already terminal (fulfilled), the cancel UPDATE flips zero
	// rows. The store MUST still write exactly one fulfillment.cancelled.v1
	// outbox row so the order saga can advance its compensation step. Before
	// the fix, PostgresStore.SaveCancel keyed the emit predicate on
	// len(cancelled) > 0 and silently dropped the event.
	store, pool := newIntegrationFulfillmentStore(t)
	ctx := context.Background()
	now := integrationFulfillmentClock()
	// Seed a single FULFILLED fulfillment_order directly — no open rows to flip.
	terminal := FulfillmentOrder{
		FulfillmentID: deterministicFulfillmentID("ORD-INT-5", "LOC-1"),
		OrderID:       "ORD-INT-5", LocationID: "LOC-1",
		Status: StatusFulfilled, CreatedAt: now,
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO fulfillment_order (
			fulfillment_id, order_id, location_id, status, created_at
		) VALUES ($1, $2, $3, $4, $5)`,
		terminal.FulfillmentID, terminal.OrderID, terminal.LocationID,
		string(terminal.Status), terminal.CreatedAt); err != nil {
		t.Fatalf("seed terminal order: %v", err)
	}
	cancelledEvent := messaging.NewEvent(EventFulfillmentCancelled, "ORD-INT-5", "corr-int-5", "",
		mustFulfillmentJSON(orderIDPayload{OrderID: "ORD-INT-5"}),
		func() time.Time { return now })
	result, err := store.SaveCancel(ctx, CancelOutcome{
		IdempotencyKey: "cancel-int-5", OrderID: "ORD-INT-5",
		Reason: "buyer_cancelled", Events: []messaging.Event{cancelledEvent},
	})
	if err != nil {
		t.Fatalf("SaveCancel error: %v", err)
	}
	if len(result.Events) != 1 || result.Events[0].Type != EventFulfillmentCancelled {
		t.Fatalf("events=%+v, want single %s (terminal projection must still acknowledge)",
			eventTypes(result.Events), EventFulfillmentCancelled)
	}
	if result.Replayed {
		t.Fatalf("first cancel of a fulfilled order must not be a replay")
	}
	// The terminal row must NOT have been re-flipped (state guard preserved).
	assertFulfillmentCount(t, pool, "outbox_event", 1)
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM fulfillment_order WHERE fulfillment_id = $1`,
		terminal.FulfillmentID).Scan(&status); err != nil {
		t.Fatalf("reload terminal order: %v", err)
	}
	if status != string(StatusFulfilled) {
		t.Fatalf("terminal order status=%q, want %q (state guard must preserve fulfilled)",
			status, StatusFulfilled)
	}
	// Replay with the same idempotency key must not duplicate the outbox row.
	replay, err := store.SaveCancel(ctx, CancelOutcome{
		IdempotencyKey: "cancel-int-5", OrderID: "ORD-INT-5",
		Reason: "buyer_cancelled", Events: []messaging.Event{cancelledEvent},
	})
	if err != nil {
		t.Fatalf("replay SaveCancel error: %v", err)
	}
	if !replay.Replayed {
		t.Fatalf("replay SaveCancel must return Replayed=true")
	}
	if len(replay.Events) != 0 {
		t.Fatalf("replay events=%d, want 0 (no duplicates)", len(replay.Events))
	}
	assertFulfillmentCount(t, pool, "outbox_event", 1)
}

func TestPostgresCancelEmitsNothingForOrderWithNoProjection(t *testing.T) {
	// Negative control: cancelling an order that has NO fulfillment_order rows
	// must stay a silent no-op (no outbox row). This is what makes an
	// order.cancelled.v1 for an order that was never paid safe to ack.
	store, pool := newIntegrationFulfillmentStore(t)
	ctx := context.Background()
	now := integrationFulfillmentClock()
	cancelledEvent := messaging.NewEvent(EventFulfillmentCancelled, "ORD-INT-6", "corr-int-6", "",
		mustFulfillmentJSON(orderIDPayload{OrderID: "ORD-INT-6"}),
		func() time.Time { return now })
	result, err := store.SaveCancel(ctx, CancelOutcome{
		IdempotencyKey: "cancel-int-6", OrderID: "ORD-INT-6",
		Reason: "buyer_cancelled", Events: []messaging.Event{cancelledEvent},
	})
	if err != nil {
		t.Fatalf("SaveCancel error: %v", err)
	}
	if !result.Replayed {
		t.Fatalf("cancel of unknown order must be a replay no-op")
	}
	if len(result.Events) != 0 {
		t.Fatalf("events=%d, want 0 (no prior fulfillment)", len(result.Events))
	}
	assertFulfillmentCount(t, pool, "outbox_event", 0)
}

func TestPostgresCompletedEventPayloadIsExact(t *testing.T) {
	store, pool := newIntegrationFulfillmentStore(t)
	ctx := context.Background()
	now := integrationFulfillmentClock()
	outcome := buildIntegrationOutcome("ORD-INT-3", "paid-int-3", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
	}, now, "corr-int-3", "cause-int-3")
	if _, err := store.SaveFulfill(ctx, outcome); err != nil {
		t.Fatalf("SaveFulfill error: %v", err)
	}
	var data []byte
	if err := pool.QueryRow(ctx, `
		SELECT payload FROM outbox_event
		WHERE subject = $1 AND event_type = $2
		ORDER BY event_id LIMIT 1`,
		"ORD-INT-3", EventFulfillmentCompleted).Scan(&data); err != nil {
		t.Fatalf("load outbox payload: %v", err)
	}
	var payload orderIDPayload
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("completed payload must decode with DisallowUnknownFields: %v", err)
	}
	if payload.OrderID != "ORD-INT-3" {
		t.Fatalf("payload order_id=%q, want ORD-INT-3", payload.OrderID)
	}
}

func TestPostgresGetFulfillmentsByOrderLoadsProjection(t *testing.T) {
	store, _ := newIntegrationFulfillmentStore(t)
	ctx := context.Background()
	now := integrationFulfillmentClock()
	outcome := buildIntegrationOutcome("ORD-INT-4", "paid-int-4", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 2},
	}, now, "corr-int-4", "")
	if _, err := store.SaveFulfill(ctx, outcome); err != nil {
		t.Fatalf("SaveFulfill error: %v", err)
	}
	orders, err := store.GetFulfillmentsByOrder(ctx, "ORD-INT-4")
	if err != nil {
		t.Fatalf("GetFulfillmentsByOrder error: %v", err)
	}
	if len(orders) != 1 || orders[0].LocationID != "LOC-1" {
		t.Fatalf("orders=%+v, want single LOC-1", orders)
	}
	items, err := store.ListItemsByFulfillment(ctx, orders[0].FulfillmentID)
	if err != nil {
		t.Fatalf("ListItemsByFulfillment error: %v", err)
	}
	if len(items) != 1 || items[0].SKU != "SKU-A" || items[0].Quantity != 2 {
		t.Fatalf("items=%+v, want SKU-A qty 2", items)
	}
	shipment, err := store.GetShipmentByFulfillment(ctx, orders[0].FulfillmentID)
	if err != nil {
		t.Fatalf("GetShipmentByFulfillment error: %v", err)
	}
	if shipment == nil || shipment.Status != ShipmentDelivered {
		t.Fatalf("shipment=%+v, want delivered", shipment)
	}
	byID, found, err := store.GetFulfillmentByID(ctx, orders[0].FulfillmentID)
	if err != nil {
		t.Fatalf("GetFulfillmentByID error: %v", err)
	}
	if !found || byID.OrderID != "ORD-INT-4" {
		t.Fatalf("byID=%+v found=%v", byID, found)
	}
}

// --- helpers ---

func integrationFulfillmentClock() time.Time {
	return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
}

func mustFulfillmentJSON(value any) json.RawMessage {
	body, _ := json.Marshal(value)
	return body
}

func buildIntegrationOutcome(
	orderID, idempotencyKey string,
	allocations allocationList,
	now time.Time,
	correlationID, causationID string,
) FulfillOutcome {
	command := FulfillCommand{
		OrderID: orderID, IdempotencyKey: idempotencyKey,
		CorrelationID: correlationID, CausationID: causationID, OccurredAt: now,
	}
	orders := buildFulfillmentOrders(command, ReservationResult{
		OrderID: orderID, Allocations: allocations,
	}, now)
	completed := messaging.NewEvent(EventFulfillmentCompleted, orderID,
		correlationID, causationID,
		mustFulfillmentJSON(orderIDPayload{OrderID: orderID}),
		func() time.Time { return now })
	return FulfillOutcome{
		IdempotencyKey: idempotencyKey, OrderID: orderID,
		FulfillmentOrders: orders, Events: []messaging.Event{completed},
	}
}

func assertFulfillmentCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		fmt.Sprintf("SELECT count(*) FROM %s", table)).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Fatalf("%s count=%d, want %d", table, count, want)
	}
}

func newIntegrationFulfillmentStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("fulfillment integration test skipped: TEST_DATABASE_URL for a dedicated PostgreSQL database is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open dedicated TEST_DATABASE_URL: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("dedicated PostgreSQL at TEST_DATABASE_URL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("task7b_fulfillment_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "fulfillment_db.sql"))
	if err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop integration schema: %v", err)
		}
		admin.Close()
	})
	return NewPostgresStore(pool, func() time.Time { return integrationFulfillmentClock() }), pool
}

var _ = errors.Is
