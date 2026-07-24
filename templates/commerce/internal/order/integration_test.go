//go:build integration

package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresCheckoutCommitIsAtomicAndIdempotent(t *testing.T) {
	store, pool := newIntegrationOrderStore(t)
	ctx := context.Background()
	cart, err := store.CreateCart(ctx, Cart{
		ID: "CART-1", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	order, event := integrationCheckoutOrder()
	commit := CheckoutCommit{
		CartID: cart.ID, CartVersion: cart.Version, RequestHash: "hash-1",
		Order: order, Event: event,
	}
	first, err := store.CommitCheckout(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CommitCheckout(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if first.OrderID != second.OrderID {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	assertOrderIntegrationCount(t, pool, "sales_order", 1)
	assertOrderIntegrationCount(t, pool, "order_item", 1)
	assertOrderIntegrationCount(t, pool, "order_customer_snapshot", 1)
	assertOrderIntegrationCount(t, pool, "checkout_request", 1)
	assertOrderIntegrationCount(t, pool, "order_saga", 1)
	assertOrderIntegrationCount(t, pool, "order_saga_step", 4)
	assertOrderIntegrationCount(t, pool, "outbox_event", 1)

	commit.RequestHash = "different"
	if _, err := store.CommitCheckout(ctx, commit); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different request error=%v", err)
	}
}

func TestPostgresCartMutationUsesCompareAndSwapVersion(t *testing.T) {
	store, _ := newIntegrationOrderStore(t)
	ctx := context.Background()
	if _, err := store.CreateCart(ctx, Cart{
		ID: "CART-RACE", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour), Lines: []CartLine{},
	}); err != nil {
		t.Fatal(err)
	}
	var successes, conflicts int
	var mu sync.Mutex
	var wait sync.WaitGroup
	for index := range 8 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.MutateCart(ctx, CartMutation{
				CartID: "CART-RACE", SKU: fmt.Sprintf("SKU-%d", index), Quantity: 1,
				ExpectedVersion: 1, Action: CartAddLine,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrVersionConflict):
				conflicts++
			default:
				t.Errorf("mutation %d: %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	if successes != 1 || conflicts != 7 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	cart, err := store.GetCart(ctx, "CART-RACE")
	if err != nil {
		t.Fatal(err)
	}
	if cart.Version != 2 || len(cart.Lines) != 1 {
		t.Fatalf("cart=%+v", cart)
	}
}

func TestPostgresInboxMakesPaymentFailureCompensationExactlyOnce(t *testing.T) {
	store, pool := newIntegrationOrderStore(t)
	ctx := context.Background()
	if _, err := store.CreateCart(ctx, Cart{
		ID: "CART-EVENT", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	order, placed := integrationCheckoutOrder()
	if _, err := store.CommitCheckout(ctx, CheckoutCommit{
		CartID: "CART-EVENT", CartVersion: 1, RequestHash: "hash-event",
		Order: order, Event: placed,
	}); err != nil {
		t.Fatal(err)
	}
	failed := messaging.NewEvent("payment.failed.v1", order.OrderID, "corr-1", "payment-attempt-1",
		json.RawMessage(`{"order_id":"ORD-1","code":"card_declined"}`),
		integrationOrderClock)
	if err := store.HandleEvent(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if err := store.HandleEvent(ctx, failed); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetOrder(ctx, order.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "cancelled" || saved.PaymentStatus != "failed" || saved.SagaState != "failed" {
		t.Fatalf("saved=%+v", saved)
	}
	assertOrderIntegrationCount(t, pool, "inbox_event", 1)
	var releases int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_event
		WHERE event_type = 'inventory.release-requested.v1'`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("release events=%d", releases)
	}
	var correlation, causation string
	if err := pool.QueryRow(ctx, `
		SELECT correlation_id, causation_id FROM outbox_event
		WHERE event_type = 'inventory.release-requested.v1'`).Scan(&correlation, &causation); err != nil {
		t.Fatal(err)
	}
	if correlation != failed.CorrelationID || causation != failed.ID {
		t.Fatalf("correlation=%q causation=%q", correlation, causation)
	}
}

func TestPostgresCancellationAfterCaptureRequestsRefundAndIgnoresLateFailure(t *testing.T) {
	store, pool := newIntegrationOrderStore(t)
	ctx := context.Background()
	if _, err := store.CreateCart(ctx, Cart{
		ID: "CART-CANCEL", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}},
	}); err != nil {
		t.Fatal(err)
	}
	order, placed := integrationCheckoutOrder()
	if _, err := store.CommitCheckout(ctx, CheckoutCommit{
		CartID: "CART-CANCEL", CartVersion: 1, RequestHash: "hash-cancel",
		Order: order, Event: placed,
	}); err != nil {
		t.Fatal(err)
	}
	paid := messaging.NewEvent("payment.captured.v1", order.OrderID, "corr-paid", "attempt-1",
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, paid); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, nil, ServiceOptions{Clock: integrationOrderClock})
	if _, err := service.Cancel(ctx, CancelCommand{
		OrderID: order.OrderID, Reason: "buyer_request", CorrelationID: "corr-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{
		"payment.refund-requested.v1",
		"inventory.release-requested.v1",
		"fulfillment.cancel-requested.v1",
	} {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM outbox_event WHERE event_type = $1`, kind).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s events=%d", kind, count)
		}
	}
	var refundStatus string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM order_saga_step
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'refund_requested'`, order.OrderID).Scan(&refundStatus); err != nil {
		t.Fatal(err)
	}
	if refundStatus != "pending" {
		t.Fatalf("refund step=%s", refundStatus)
	}
	lateFailure := messaging.NewEvent("payment.failed.v1", order.OrderID, "corr-paid", "attempt-2",
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, lateFailure); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetOrder(ctx, order.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "cancelled" || saved.PaymentStatus != "paid" {
		t.Fatalf("saved=%+v", saved)
	}
}

func integrationCheckoutOrder() (Order, messaging.Event) {
	now := integrationOrderClock()
	order := Order{
		OrderID: "ORD-1", Number: "O0001", CustomerID: "CUS-1",
		Status: "pending", PaymentStatus: "pending", FulfillmentStatus: "unfulfilled",
		Currency: "CNY", SubtotalMinor: 2472, ShippingMinor: 800, TotalMinor: 3272,
		Customer: CustomerSnapshot{
			ID: "CUS-1", Email: "buyer@example.test", Name: "Buyer", Phone: "13800000000",
			Address: json.RawMessage(`{"address_id":"ADDR-1","recipient":"Buyer"}`),
		},
		ShippingAddress: json.RawMessage(`{"address_id":"ADDR-1","recipient":"Buyer"}`),
		Lines: []OrderLine{{
			ID: "ITEM-1", SKU: "SKU-1", Title: "Snapshot", Quantity: 2,
			UnitPriceMinor: 1236, TotalMinor: 2472,
		}},
		IdempotencyKey: "checkout-1", PlacedAt: now, SagaState: "paying",
	}
	payload, _ := json.Marshal(orderPlacedPayload(order))
	return order, messaging.NewEvent("order.placed.v1", order.OrderID, "corr-1", "", payload, integrationOrderClock)
}

func integrationOrderClock() time.Time {
	return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
}

func newIntegrationOrderStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("order integration test skipped: TEST_DATABASE_URL for a dedicated PostgreSQL database is not set")
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
	schema := fmt.Sprintf("task6_order_%d", time.Now().UnixNano())
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
	migration, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "order_db.sql"))
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
	return NewPostgresStore(pool, integrationOrderClock), pool
}

func assertOrderIntegrationCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s rows=%d, want %d", table, got, want)
	}
}
