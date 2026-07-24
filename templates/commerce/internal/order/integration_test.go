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
	assertOrderIntegrationCount(t, pool, "order_item_snapshot", 1)
	assertOrderIntegrationCount(t, pool, "order_payment_projection", 1)
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
				IdempotencyKey: fmt.Sprintf("mutation-%d", index),
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

func TestPostgresCheckoutClaimIsSingleOwnerAndCartCASCompensates(t *testing.T) {
	store, pool := newIntegrationOrderStore(t)
	ctx := context.Background()
	cart, err := store.CreateCart(ctx, Cart{
		ID: "CART-CLAIM", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 1}}, IdempotencyKey: "create-claim",
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := CheckoutClaim{
		IdempotencyKey: "checkout-claim", RequestHash: "hash-claim", Cart: cart,
		OrderID: "ORD-CLAIM", Now: integrationOrderClock(),
	}
	var owners int
	var wait sync.WaitGroup
	var mu sync.Mutex
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, owned, err := store.ClaimCheckout(ctx, claim)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if owned {
				mu.Lock()
				owners++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if owners != 1 {
		t.Fatalf("checkout owners=%d, want 1", owners)
	}
	if _, _, err := store.ClaimCheckout(ctx, CheckoutClaim{
		IdempotencyKey: claim.IdempotencyKey, RequestHash: "different", Cart: cart,
		OrderID: claim.OrderID, Now: claim.Now,
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed fingerprint error=%v", err)
	}
	if _, err := store.MutateCart(ctx, CartMutation{
		CartID: cart.ID, SKU: "SKU-1", Quantity: 2, ExpectedVersion: 1,
		Action: CartChangeLine, IdempotencyKey: "mutate-after-claim",
	}); err != nil {
		t.Fatal(err)
	}
	order, placed := integrationCheckoutOrder()
	order.OrderID, order.Number, order.IdempotencyKey = claim.OrderID, "OCLAIM", claim.IdempotencyKey
	if err := store.SaveCheckoutPrepared(ctx, claim.IdempotencyKey, claim.RequestHash,
		order, ReservationCommand{OrderID: order.OrderID, IdempotencyKey: "inventory:claim",
			Lines: []ReservationLine{{SKU: "SKU-1", Quantity: 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCheckoutReserved(ctx, claim.IdempotencyKey, claim.RequestHash,
		ReservationResult{OrderID: order.OrderID, Allocations: []ReservationAllocation{{
			AllocationID: "RES-CLAIM", SKU: "SKU-1", Quantity: 1, Status: "active",
		}}}); err != nil {
		t.Fatal(err)
	}
	placed.Subject = order.OrderID
	if _, err := store.CommitCheckout(ctx, CheckoutCommit{
		CartID: cart.ID, CartVersion: 1, RequestHash: claim.RequestHash,
		Order: order, Event: placed,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("cart-vs-checkout commit error=%v", err)
	}
	var orders int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sales_order`).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Fatalf("orders after cart CAS rollback=%d", orders)
	}
}

func TestPostgresConcurrentCheckoutSameKeyReturnsOneOrderAndOneReservation(t *testing.T) {
	store, _ := newIntegrationOrderStore(t)
	ctx := context.Background()
	if _, err := store.CreateCart(ctx, Cart{
		ID: "CART-CHECKOUT-RACE", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}}, IdempotencyKey: "create-checkout-race",
	}); err != nil {
		t.Fatal(err)
	}
	customer := &customerStub{snapshot: CustomerSnapshot{
		ID: "CUS-1", Email: "buyer@example.test", Name: "Buyer",
		Address: json.RawMessage(`{"address_id":"ADDR-1","country_code":"CN","province":"上海市"}`),
	}}
	catalog := &catalogStub{items: []CatalogSnapshot{{
		ProductID: "PROD-1", SKU: "SKU-1", ProductTitle: "Product",
		VariantTitle: "Variant", Title: "Product — Variant", UnitPriceMinor: 1236,
		Currency: "CNY", Channel: "web",
	}}}
	inventory := &inventoryStub{keys: map[string]string{}}
	service := NewService(store, customer, catalog, inventory, ServiceOptions{Clock: integrationOrderClock})
	command := CheckoutCommand{
		CartID: "CART-CHECKOUT-RACE", AddressID: "ADDR-1",
		IdempotencyKey: "checkout-race", CorrelationID: "race",
	}
	type result struct {
		order Order
		err   error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			order, err := service.Checkout(ctx, command)
			results <- result{order: order, err: err}
		}()
	}
	wait.Wait()
	close(results)
	var orderID string
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent checkout: %v", result.err)
		}
		if orderID == "" {
			orderID = result.order.OrderID
		} else if result.order.OrderID != orderID {
			t.Fatalf("checkout order IDs %s and %s", orderID, result.order.OrderID)
		}
	}
	if inventory.uniqueReservations != 1 {
		t.Fatalf("unique reservations=%d, want 1", inventory.uniqueReservations)
	}
}

func TestPostgresEventPayloadMismatchRollsBackDomainAndOutbox(t *testing.T) {
	store, pool := newIntegrationOrderStore(t)
	ctx := context.Background()
	if _, err := store.CreateCart(ctx, Cart{
		ID: "CART-STRICT", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}}, IdempotencyKey: "create-strict",
	}); err != nil {
		t.Fatal(err)
	}
	order, placed := integrationCheckoutOrder()
	if _, err := store.CommitCheckout(ctx, CheckoutCommit{
		CartID: "CART-STRICT", CartVersion: 1, RequestHash: "strict",
		Order: order, Event: placed,
	}); err != nil {
		t.Fatal(err)
	}
	bad := messaging.NewEvent("payment.captured.v1", order.OrderID, "corr", "payment",
		json.RawMessage(`{"order_id":"OTHER","currency":"CNY","amount_minor":3272}`),
		integrationOrderClock)
	if err := store.HandleEvent(ctx, bad); err == nil {
		t.Fatal("mismatched payload was accepted")
	}
	saved, err := store.GetOrder(ctx, order.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "pending" || saved.PaymentStatus != "pending" {
		t.Fatalf("domain changed after invalid event: %+v", saved)
	}
	var commits int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_event
		WHERE event_type = 'inventory.commit-requested.v1'`).Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Fatalf("commit outbox rows after invalid event=%d", commits)
	}
}

func TestPostgresCancelVersusFulfillmentEventSerializesToOneTerminalState(t *testing.T) {
	store, _ := newIntegrationOrderStore(t)
	ctx := context.Background()
	if _, err := store.CreateCart(ctx, Cart{
		ID: "CART-CANCEL-RACE", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: integrationOrderClock().Add(time.Hour),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}}, IdempotencyKey: "create-cancel-race",
	}); err != nil {
		t.Fatal(err)
	}
	order, placed := integrationCheckoutOrder()
	if _, err := store.CommitCheckout(ctx, CheckoutCommit{
		CartID: "CART-CANCEL-RACE", CartVersion: 1, RequestHash: "cancel-race",
		Order: order, Event: placed,
	}); err != nil {
		t.Fatal(err)
	}
	captured := messaging.NewEvent("payment.captured.v1", order.OrderID, "race", "payment",
		json.RawMessage(`{"order_id":"ORD-1","currency":"CNY","amount_minor":3272}`),
		integrationOrderClock)
	if err := store.HandleEvent(ctx, captured); err != nil {
		t.Fatal(err)
	}
	committed := messaging.NewEvent("inventory.committed.v1", order.OrderID, "race", captured.ID,
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, committed); err != nil {
		t.Fatal(err)
	}
	completed := messaging.NewEvent("fulfillment.completed.v1", order.OrderID, "race", committed.ID,
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	service := NewService(store, nil, nil, nil, ServiceOptions{Clock: integrationOrderClock})
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		errorsSeen <- store.HandleEvent(ctx, completed)
	}()
	go func() {
		defer wait.Done()
		_, err := service.Cancel(ctx, CancelCommand{
			OrderID: order.OrderID, Reason: "race", IdempotencyKey: "cancel-race",
			CorrelationID: "race",
		})
		errorsSeen <- err
	}()
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil && !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("race error=%v", err)
		}
	}
	saved, err := store.GetOrder(ctx, order.OrderID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "completed" && saved.Status != "cancelled" {
		t.Fatalf("race terminal state=%s", saved.Status)
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
	released := messaging.NewEvent("inventory.released.v1", order.OrderID, "corr-1", failed.ID,
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, released); err != nil {
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
		json.RawMessage(`{"order_id":"ORD-1","currency":"CNY","amount_minor":3272}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, paid); err != nil {
		t.Fatal(err)
	}
	committed := messaging.NewEvent("inventory.committed.v1", order.OrderID, "corr-paid", paid.ID,
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, committed); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, nil, ServiceOptions{Clock: integrationOrderClock})
	if _, err := service.Cancel(ctx, CancelCommand{
		OrderID: order.OrderID, Reason: "buyer_request", IdempotencyKey: "cancel-1",
		CorrelationID: "corr-cancel",
	}); err != nil {
		t.Fatal(err)
	}
	replay, err := service.Cancel(ctx, CancelCommand{
		OrderID: order.OrderID, Reason: "buyer_request", IdempotencyKey: "cancel-1",
		CorrelationID: "corr-cancel",
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("cancellation replay=%+v err=%v", replay, err)
	}
	if _, err := service.Cancel(ctx, CancelCommand{
		OrderID: order.OrderID, Reason: "different", IdempotencyKey: "cancel-1",
		CorrelationID: "corr-cancel",
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("cancellation fingerprint error=%v", err)
	}
	for _, kind := range []string{"fulfillment.cancel-requested.v1"} {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM outbox_event WHERE event_type = $1`, kind).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s events=%d", kind, count)
		}
	}
	for _, kind := range []string{"payment.refund-requested.v1", "inventory.release-requested.v1"} {
		var count int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM outbox_event WHERE event_type = $1`, kind).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s emitted before fulfillment cancellation result", kind)
		}
	}
	cancelled := messaging.NewEvent("fulfillment.cancelled.v1", order.OrderID, "corr-cancel", committed.ID,
		json.RawMessage(`{"order_id":"ORD-1"}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, cancelled); err != nil {
		t.Fatal(err)
	}
	partial := messaging.NewEvent("refund.succeeded.v1", order.OrderID, "corr-cancel", cancelled.ID,
		json.RawMessage(`{"order_id":"ORD-1","currency":"CNY","amount_minor":1000}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, partial); err != nil {
		t.Fatal(err)
	}
	partiallyRefunded, err := store.GetOrder(ctx, order.OrderID)
	if err != nil || partiallyRefunded.PaymentStatus != "partially_refunded" {
		t.Fatalf("partial refund order=%+v err=%v", partiallyRefunded, err)
	}
	refunded := messaging.NewEvent("refund.succeeded.v1", order.OrderID, "corr-cancel", partial.ID,
		json.RawMessage(`{"order_id":"ORD-1","currency":"CNY","amount_minor":2272}`), integrationOrderClock)
	if err := store.HandleEvent(ctx, refunded); err != nil {
		t.Fatal(err)
	}
	var releases int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM outbox_event
		WHERE event_type = 'inventory.release-requested.v1'`).Scan(&releases); err != nil {
		t.Fatal(err)
	}
	if releases != 1 {
		t.Fatalf("release events after refund=%d", releases)
	}
	var refundStatus string
	if err := pool.QueryRow(ctx, `
		SELECT status FROM order_saga_step
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'refund_requested'`, order.OrderID).Scan(&refundStatus); err != nil {
		t.Fatal(err)
	}
	if refundStatus != "completed" {
		t.Fatalf("refund step=%s", refundStatus)
	}
	lateFailure := messaging.NewEvent("payment.failed.v1", order.OrderID, "corr-paid", "attempt-2",
		json.RawMessage(`{"order_id":"ORD-1","code":"late"}`), integrationOrderClock)
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
