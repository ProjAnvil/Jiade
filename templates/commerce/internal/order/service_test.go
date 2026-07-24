package order

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	platformclient "commerce/internal/platform/client"
	"commerce/internal/platform/messaging"
	amqp "github.com/rabbitmq/amqp091-go"
)

func TestCheckoutSnapshotsPricesAndWritesOutboxAtomically(t *testing.T) {
	fixture := newCheckoutFixture()
	got, err := fixture.service.Checkout(context.Background(), CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", IdempotencyKey: "checkout-1",
		CorrelationID: "request-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TotalMinor != 3593 || got.TaxMinor != 321 {
		t.Fatalf("total=%d tax=%d, want 3593 and 321", got.TotalMinor, got.TaxMinor)
	}
	saved := fixture.store.orders[got.OrderID]
	if saved.Lines[0].SKU != "SKU-1" || saved.Lines[0].Title != "Snapshot title" ||
		saved.Lines[0].UnitPriceMinor != 1236 || saved.Customer.Email != "buyer@example.test" {
		t.Fatalf("order did not retain immutable snapshots: %+v", saved)
	}
	assertEventCount(t, fixture.store.events, "order.placed.v1", 1)
	if fixture.store.atomicCommits != 1 {
		t.Fatalf("atomic commits=%d, want 1", fixture.store.atomicCommits)
	}
}

func TestCheckoutReplayAndConflictUseRequestFingerprint(t *testing.T) {
	fixture := newCheckoutFixture()
	command := CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", IdempotencyKey: "checkout-1",
		CorrelationID: "request-1",
	}
	first, err := fixture.service.Checkout(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.service.Checkout(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.OrderID != second.OrderID || fixture.inventory.calls != 1 ||
		fixture.customer.calls != 1 || fixture.catalog.calls != 1 {
		t.Fatalf("first=%+v second=%+v calls customer=%d catalog=%d inventory=%d",
			first, second, fixture.customer.calls, fixture.catalog.calls, fixture.inventory.calls)
	}
	command.AddressID = "ADDR-2"
	if _, err := fixture.service.Checkout(context.Background(), command); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different request error=%v, want ErrIdempotencyConflict", err)
	}
}

func TestCheckoutReentryAfterReservedInventoryReusesDeterministicIdentity(t *testing.T) {
	fixture := newCheckoutFixture()
	fixture.store.failCommitOnce = true
	command := CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", IdempotencyKey: "checkout-1",
		CorrelationID: "request-1",
	}
	if _, err := fixture.service.Checkout(context.Background(), command); !errors.Is(err, ErrCheckoutUncertain) {
		t.Fatalf("first checkout error=%v, want ErrCheckoutUncertain", err)
	}
	got, err := fixture.service.Checkout(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.inventory.uniqueReservations != 1 || fixture.inventory.lastOrderID != got.OrderID {
		t.Fatalf("reservations=%d last order=%s got order=%s",
			fixture.inventory.uniqueReservations, fixture.inventory.lastOrderID, got.OrderID)
	}
	assertEventCount(t, fixture.store.events, "order.placed.v1", 1)
}

func TestCheckoutRejectsMismatchedCustomerSnapshotBeforeReservation(t *testing.T) {
	fixture := newCheckoutFixture()
	fixture.customer.snapshot.ID = "CUS-OTHER"
	_, err := fixture.service.Checkout(context.Background(), CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", IdempotencyKey: "checkout-1",
		CorrelationID: "request-1",
	})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("checkout error=%v, want ErrInvalidCommand", err)
	}
	if fixture.inventory.calls != 0 {
		t.Fatalf("inventory calls=%d, want 0", fixture.inventory.calls)
	}
}

func TestCheckoutWelcome10AllocatesDiscountAndRegionalTaxExactly(t *testing.T) {
	fixture := newCheckoutFixture()
	order, err := fixture.service.Checkout(context.Background(), CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", CouponCode: "welcome10",
		IdempotencyKey: "checkout-coupon", CorrelationID: "request-coupon",
	})
	if err != nil {
		t.Fatal(err)
	}
	if order.DiscountMinor != 247 || order.TaxMinor != 289 || order.TotalMinor != 3314 {
		t.Fatalf("coupon totals discount=%d tax=%d total=%d", order.DiscountMinor, order.TaxMinor, order.TotalMinor)
	}
	if order.Lines[0].DiscountMinor != 247 || order.Lines[0].TaxMinor != 289 {
		t.Fatalf("coupon line=%+v", order.Lines[0])
	}
}

func TestReservationResultRejectsTerminalAllocation(t *testing.T) {
	order := Order{OrderID: "ORD-1"}
	command := ReservationCommand{
		OrderID: "ORD-1", Lines: []ReservationLine{{SKU: "SKU-1", Quantity: 1}},
	}
	err := validateReservationResult(order, command, ReservationResult{
		OrderID: "ORD-1", Allocations: []ReservationAllocation{{
			AllocationID: "RES-1", SKU: "SKU-1", Quantity: 1, Status: "released",
		}},
	})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("terminal reservation error=%v", err)
	}
}

func TestPaymentFailureCancelsAndEmitsInventoryReleaseOnce(t *testing.T) {
	fixture := newCheckoutFixture()
	order, err := fixture.service.Checkout(context.Background(), CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", IdempotencyKey: "checkout-1",
		CorrelationID: "request-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	event := messaging.Event{
		ID: "00000000-0000-4000-8000-000000000001", SchemaVersion: 1,
		Type: "payment.failed.v1", Subject: order.OrderID, CorrelationID: "request-1",
		Data: json.RawMessage(`{"order_id":"` + order.OrderID + `","code":"card_declined"}`),
	}
	if err := fixture.store.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.HandleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	saved := fixture.store.orders[order.OrderID]
	if saved.Status != "cancelled" || saved.PaymentStatus != "failed" || saved.SagaState != "failed" {
		t.Fatalf("state=%+v", saved)
	}
	assertEventCount(t, fixture.store.events, "inventory.release-requested.v1", 1)
	assertEventCausation(t, fixture.store.events, "inventory.release-requested.v1", event.ID)
}

func TestPaidThenCancelledStartsWithFulfillmentCancellation(t *testing.T) {
	fixture := newCheckoutFixture()
	placed, err := fixture.service.Checkout(context.Background(), CheckoutCommand{
		CartID: "CART-1", AddressID: "ADDR-1", IdempotencyKey: "checkout-1",
		CorrelationID: "request-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	paid := messaging.Event{
		ID: "00000000-0000-4000-8000-000000000002", SchemaVersion: 1,
		Type: "payment.captured.v1", Subject: placed.OrderID, CorrelationID: "request-1",
		Data: json.RawMessage(`{"order_id":"` + placed.OrderID + `"}`),
	}
	if err := fixture.store.HandleEvent(context.Background(), paid); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Cancel(context.Background(), CancelCommand{
		OrderID: placed.OrderID, Reason: "buyer_request", IdempotencyKey: "cancel-1",
		CorrelationID: "cancel-1",
	}); err != nil {
		t.Fatal(err)
	}
	assertEventCount(t, fixture.store.events, "order.paid.v1", 1)
	assertEventCount(t, fixture.store.events, "payment.refund-requested.v1", 0)
	assertEventCount(t, fixture.store.events, "inventory.release-requested.v1", 0)
	assertEventCount(t, fixture.store.events, "fulfillment.cancel-requested.v1", 1)
	cancelledID := ""
	for _, event := range fixture.store.events {
		if event.Type == "order.cancelled.v1" {
			cancelledID = event.ID
		}
	}
	if cancelledID == "" {
		t.Fatal("order cancellation event missing")
	}
	assertEventCausation(t, fixture.store.events, "fulfillment.cancel-requested.v1", cancelledID)

	lateFailure := messaging.Event{
		ID: "00000000-0000-4000-8000-000000000003", SchemaVersion: 1,
		Type: "payment.failed.v1", Subject: placed.OrderID, CorrelationID: "request-1",
		Data: json.RawMessage(`{"order_id":"` + placed.OrderID + `"}`),
	}
	if err := fixture.store.HandleEvent(context.Background(), lateFailure); err != nil {
		t.Fatal(err)
	}
	if fixture.store.orders[placed.OrderID].PaymentStatus != "paid" {
		t.Fatal("late failure moved captured payment backward")
	}
}

func TestCancellationRejectsCompletedOrderWithoutCompensation(t *testing.T) {
	fixture := newCheckoutFixture()
	fixture.store.orders["ORD-DONE"] = Order{
		OrderID: "ORD-DONE", Status: "completed", PaymentStatus: "paid",
		FulfillmentStatus: "fulfilled",
	}
	_, err := fixture.service.Cancel(context.Background(), CancelCommand{
		OrderID: "ORD-DONE", Reason: "too_late", IdempotencyKey: "cancel-done",
		CorrelationID: "cancel-done",
	})
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("cancel error=%v, want ErrInvalidCommand", err)
	}
	assertEventCount(t, fixture.store.events, "payment.refund-requested.v1", 0)
}

func TestHTTPDependencyClientsPropagateHeadersAndDecodeSnapshots(t *testing.T) {
	var calls []string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(body))
		for name, want := range map[string]string{
			"X-Request-ID": "request-1", "traceparent": "trace-1",
			"Idempotency-Key": "checkout-1", "X-Correlation-ID": "corr-1",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s=%q, want %q", name, got, want)
			}
		}
		status := http.StatusOK
		responseBody := ""
		switch r.URL.Path {
		case "/internal/v1/customer-addresses/validate":
			responseBody = `{"valid":true,"customer":{"customer_id":"CUS-1","status":"active"},"address":{"address_id":"ADDR-1","recipient":"Buyer","country_code":"CN"}}`
		case "/api/v1/customers/CUS-1":
			responseBody = `{"customer_id":"CUS-1","email":"buyer@example.test","name":"Buyer","phone":"13800000000","status":"active"}`
		case "/internal/v1/catalog/skus/SKU-1":
			responseBody = `{"sku":"SKU-1","title":"Snapshot","status":"active","available_for_sale":true,"unit_price_minor":1236,"currency":"CNY"}`
		case "/internal/v1/reservations":
			status = http.StatusCreated
			responseBody = `{"order_id":"ORD-1","allocations":[]}`
		case "/internal/v1/reservations/ORD-1/release":
			responseBody = `{"order_id":"ORD-1","allocations":[]}`
		default:
			status = http.StatusNotFound
		}
		return &http.Response{
			StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(responseBody)), Request: r,
		}, nil
	})
	resilient := platformclient.New(platformclient.Config{
		HTTPClient: &http.Client{Transport: transport},
	})
	baseURL := "http://dependency.test"
	propagation := Propagation{
		RequestID: "request-1", Traceparent: "trace-1", IdempotencyKey: "checkout-1",
		CorrelationID: "corr-1",
	}
	customer, err := NewCustomerHTTPClient(baseURL, resilient).Validate(
		context.Background(), "CUS-1", "ADDR-1", propagation)
	if err != nil {
		t.Fatal(err)
	}
	if customer.Email != "buyer@example.test" || !strings.Contains(string(customer.Address), `"address_id":"ADDR-1"`) {
		t.Fatalf("customer=%+v", customer)
	}
	snapshots, err := NewCatalogHTTPClient(baseURL, resilient).Snapshot(
		context.Background(), []string{"SKU-1"}, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].UnitPriceMinor != 1236 {
		t.Fatalf("snapshots=%+v", snapshots)
	}
	inventory := NewInventoryHTTPClient(baseURL, resilient)
	if err := inventory.Reserve(context.Background(), ReservationCommand{
		OrderID: "ORD-1", IdempotencyKey: "checkout-1",
		Lines: []ReservationLine{{SKU: "SKU-1", Quantity: 2}},
	}, propagation); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Release(context.Background(), "ORD-1", propagation); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestCatalogClientRejectsSnapshotNotAvailableForSale(t *testing.T) {
	_, err := (catalogSnapshotResponse{
		SKU: "SKU-1", Title: "Retired", Status: "inactive",
		AvailableForSale: false, UnitPriceMinor: 100, Currency: "CNY",
	}).snapshotFor("SKU-1")
	if !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("snapshot error=%v, want ErrInvalidCommand", err)
	}
}

func TestOrderRuntimeReadinessTracksDatabaseBrokerPublisherAndWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	relay := NewWorkerLifecycle(ctx, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	consumer := NewWorkerLifecycle(ctx, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	ready := NewRuntimeReadiness(
		func(context.Context) error { return nil },
		availabilityStub(true),
		func() bool { return false },
		relay,
		consumer,
	)
	if err := ready(context.Background()); err != nil {
		t.Fatalf("ready error=%v", err)
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	if err := relay.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if err := ready(context.Background()); err == nil {
		t.Fatal("readiness remained true after workers stopped")
	}
}

func TestRetryAcknowledgerSeparatesTransientRetryFromTerminalDLQ(t *testing.T) {
	original := &acknowledgerStub{}
	publisher := &retryPublisherStub{}
	delivery := amqp.Delivery{
		Acknowledger: original, DeliveryTag: 7, ContentType: "application/json",
		DeliveryMode: amqp.Persistent, MessageId: "event-1", Type: "payment.failed.v1",
		Body: []byte(`{"id":"event-1"}`), Headers: amqp.Table{"attempt": int32(1)},
	}
	routing := DeliveryRouting{
		RetryExchange: "retry.exchange", RetryRoutingKey: "retry",
		DeadExchange: "dead.exchange", DeadRoutingKey: "dead",
	}
	transient := &retryAcknowledger{
		original: original, publisher: publisher, delivery: delivery, routing: routing,
	}
	if err := transient.Nack(delivery.DeliveryTag, false, false); err != nil {
		t.Fatal(err)
	}
	if publisher.exchange != routing.RetryExchange || publisher.key != routing.RetryRoutingKey ||
		original.acks != 1 || original.nacks != 0 {
		t.Fatalf("transient publisher=%+v acknowledger=%+v", publisher, original)
	}

	original = &acknowledgerStub{}
	publisher = &retryPublisherStub{}
	terminal := &retryAcknowledger{
		original: original, publisher: publisher, delivery: delivery, routing: routing,
	}
	if err := terminal.Reject(delivery.DeliveryTag, false); err != nil {
		t.Fatal(err)
	}
	if publisher.exchange != routing.DeadExchange || publisher.key != routing.DeadRoutingKey ||
		original.acks != 1 || original.rejects != 0 {
		t.Fatalf("terminal publisher=%+v acknowledger=%+v", publisher, original)
	}
}

func TestRetryAcknowledgerDoesNotAckUnconfirmedRoute(t *testing.T) {
	original := &acknowledgerStub{}
	publisher := &retryPublisherStub{err: errors.New("negative confirmation")}
	delivery := amqp.Delivery{
		Acknowledger: original, DeliveryTag: 8, MessageId: "event-8",
		Body: []byte(`{"id":"event-8"}`),
	}
	acknowledger := &retryAcknowledger{
		original: original, publisher: publisher, delivery: delivery,
		routing: DeliveryRouting{RetryExchange: "retry", RetryRoutingKey: "retry"},
	}
	if err := acknowledger.Nack(delivery.DeliveryTag, false, false); err == nil {
		t.Fatal("unconfirmed retry route returned nil")
	}
	if original.acks != 0 {
		t.Fatalf("original acks=%d, want zero", original.acks)
	}
}

type acknowledgerStub struct {
	acks, nacks, rejects int
}

func (acknowledger *acknowledgerStub) Ack(uint64, bool) error {
	acknowledger.acks++
	return nil
}

func (acknowledger *acknowledgerStub) Nack(uint64, bool, bool) error {
	acknowledger.nacks++
	return nil
}

func (acknowledger *acknowledgerStub) Reject(uint64, bool) error {
	acknowledger.rejects++
	return nil
}

type retryPublisherStub struct {
	exchange string
	key      string
	err      error
}

func (publisher *retryPublisherStub) Route(
	_ context.Context,
	exchange string,
	key string,
	_ amqp.Publishing,
) error {
	publisher.exchange, publisher.key = exchange, key
	return publisher.err
}

type availabilityStub bool

func (available availabilityStub) Available() bool { return bool(available) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type checkoutFixture struct {
	service   *Service
	store     *memoryOrderStore
	customer  *customerStub
	catalog   *catalogStub
	inventory *inventoryStub
}

func newCheckoutFixture() checkoutFixture {
	store := newMemoryOrderStore()
	store.carts["CART-1"] = Cart{
		ID: "CART-1", CustomerID: "CUS-1", Status: CartActive, Currency: "CNY",
		Version: 1, ExpiresAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
		Lines: []CartLine{{SKU: "SKU-1", Quantity: 2}},
	}
	customer := &customerStub{snapshot: CustomerSnapshot{
		ID: "CUS-1", Email: "buyer@example.test", Name: "Buyer", Phone: "13800000000",
		Address: json.RawMessage(`{"address_id":"ADDR-1","recipient":"Buyer","country_code":"CN","province":"上海市","city":"上海市","district":"浦东新区","line1":"世纪大道 1 号","postal_code":"200000"}`),
	}}
	catalog := &catalogStub{items: []CatalogSnapshot{{
		SKU: "SKU-1", Title: "Snapshot title", UnitPriceMinor: 1236, Currency: "CNY",
	}}}
	inventory := &inventoryStub{keys: map[string]string{}}
	service := NewService(store, customer, catalog, inventory, ServiceOptions{
		Clock: func() time.Time { return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC) },
	})
	return checkoutFixture{service: service, store: store, customer: customer, catalog: catalog, inventory: inventory}
}

type memoryOrderStore struct {
	mu             sync.Mutex
	carts          map[string]Cart
	orders         map[string]Order
	requests       map[string]string
	events         []messaging.Event
	inbox          map[string]bool
	atomicCommits  int
	failCommitOnce bool
	cartCommands   map[string]memoryCartCommand
	cancelCommands map[string]string
}

type memoryCartCommand struct {
	hash string
	cart Cart
}

func newMemoryOrderStore() *memoryOrderStore {
	return &memoryOrderStore{
		carts: map[string]Cart{}, orders: map[string]Order{}, requests: map[string]string{},
		inbox: map[string]bool{}, cartCommands: map[string]memoryCartCommand{},
		cancelCommands: map[string]string{},
	}
}

func (store *memoryOrderStore) CreateCart(_ context.Context, cart Cart) (Cart, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	hash := commandFingerprint(struct{ CustomerID, Currency string }{cart.CustomerID, cart.Currency})
	if prior, ok := store.cartCommands["create:"+cart.IdempotencyKey]; ok {
		if prior.hash != hash {
			return Cart{}, ErrIdempotencyConflict
		}
		replay := prior.cart
		replay.Replayed = true
		return replay, nil
	}
	store.carts[cart.ID] = cart
	store.cartCommands["create:"+cart.IdempotencyKey] = memoryCartCommand{hash: hash, cart: cart}
	return cart, nil
}

func (store *memoryOrderStore) GetCart(_ context.Context, id string) (Cart, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	cart, ok := store.carts[id]
	if !ok {
		return Cart{}, ErrCartNotFound
	}
	return cart, nil
}

func (store *memoryOrderStore) MutateCart(_ context.Context, command CartMutation) (Cart, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	hash := commandFingerprint(struct {
		SKU               string
		Quantity, Version int64
		Action            CartMutationAction
	}{command.SKU, command.Quantity, command.ExpectedVersion, command.Action})
	commandKey := "mutate:" + command.CartID + ":" + command.IdempotencyKey
	if prior, ok := store.cartCommands[commandKey]; ok {
		if prior.hash != hash {
			return Cart{}, ErrIdempotencyConflict
		}
		replay := prior.cart
		replay.Replayed = true
		return replay, nil
	}
	cart := store.carts[command.CartID]
	if cart.Version != command.ExpectedVersion {
		return Cart{}, ErrVersionConflict
	}
	switch command.Action {
	case CartAddLine:
		for _, line := range cart.Lines {
			if line.SKU == command.SKU {
				return Cart{}, ErrInvalidCommand
			}
		}
		cart.Lines = append(cart.Lines, CartLine{SKU: command.SKU, Quantity: command.Quantity})
	case CartChangeLine:
		found := false
		for index := range cart.Lines {
			if cart.Lines[index].SKU == command.SKU {
				cart.Lines[index].Quantity = command.Quantity
				found = true
			}
		}
		if !found {
			return Cart{}, ErrInvalidCommand
		}
	case CartRemoveLine:
		found := false
		lines := cart.Lines[:0]
		for _, line := range cart.Lines {
			if line.SKU == command.SKU {
				found = true
				continue
			}
			lines = append(lines, line)
		}
		if !found {
			return Cart{}, ErrInvalidCommand
		}
		cart.Lines = lines
	}
	cart.Version++
	store.carts[command.CartID] = cart
	store.cartCommands[commandKey] = memoryCartCommand{hash: hash, cart: cart}
	return cart, nil
}

func (store *memoryOrderStore) FindCheckout(_ context.Context, key string) (CheckoutRecord, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	hash, ok := store.requests[key]
	if !ok {
		return CheckoutRecord{}, false, nil
	}
	for _, order := range store.orders {
		if order.IdempotencyKey == key {
			return CheckoutRecord{RequestHash: hash, Order: order}, true, nil
		}
	}
	return CheckoutRecord{}, false, nil
}

func (store *memoryOrderStore) CommitCheckout(_ context.Context, commit CheckoutCommit) (Order, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.failCommitOnce {
		store.failCommitOnce = false
		return Order{}, errors.New("database unavailable after reservation")
	}
	if hash, exists := store.requests[commit.Order.IdempotencyKey]; exists {
		if hash != commit.RequestHash {
			return Order{}, ErrIdempotencyConflict
		}
		return store.orders[commit.Order.OrderID], nil
	}
	cart := store.carts[commit.CartID]
	if cart.Version != commit.CartVersion || cart.Status != CartActive {
		return Order{}, ErrVersionConflict
	}
	store.requests[commit.Order.IdempotencyKey] = commit.RequestHash
	store.orders[commit.Order.OrderID] = commit.Order
	cart.Status = CartConverted
	store.carts[commit.CartID] = cart
	store.events = append(store.events, commit.Event)
	store.atomicCommits++
	return commit.Order, nil
}

func (store *memoryOrderStore) ListOrders(context.Context, OrderCursor, int) ([]Order, error) {
	return nil, nil
}

func (store *memoryOrderStore) GetOrder(_ context.Context, id string) (Order, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	order, ok := store.orders[id]
	if !ok {
		return Order{}, ErrOrderNotFound
	}
	return order, nil
}

func (store *memoryOrderStore) CancelOrder(_ context.Context, command CancelCommand, events []messaging.Event) (Order, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	order, ok := store.orders[command.OrderID]
	if !ok {
		return Order{}, ErrOrderNotFound
	}
	hash := commandFingerprint(struct{ OrderID, Reason string }{command.OrderID, command.Reason})
	if prior, ok := store.cancelCommands[command.IdempotencyKey]; ok {
		if prior != hash {
			return Order{}, ErrIdempotencyConflict
		}
		order.Replayed = true
		return order, nil
	}
	if order.Status == "cancelled" {
		return order, nil
	}
	if order.Status != "pending" && order.Status != "confirmed" {
		return Order{}, ErrInvalidCommand
	}
	cancelled := newOrderEvent("order.cancelled.v1", order, command.CorrelationID, command.CausationID,
		map[string]any{"order_id": order.OrderID, "reason": command.Reason}, time.Now().UTC())
	events = []messaging.Event{cancelled}
	if order.PaymentStatus == "paid" || order.PaymentStatus == "authorized" ||
		order.PaymentStatus == "partially_refunded" {
		events = append(events, newOrderEvent(
			"fulfillment.cancel-requested.v1", order, command.CorrelationID, cancelled.ID,
			map[string]any{"order_id": order.OrderID}, time.Now().UTC()))
	} else {
		events = append(events, newOrderEvent(
			"inventory.release-requested.v1", order, command.CorrelationID, cancelled.ID,
			map[string]any{"order_id": order.OrderID}, time.Now().UTC()))
	}
	order.Status = "cancelled"
	order.SagaState = "compensating"
	store.orders[command.OrderID] = order
	store.events = append(store.events, events...)
	store.cancelCommands[command.IdempotencyKey] = hash
	return order, nil
}

func (store *memoryOrderStore) HandleEvent(_ context.Context, event messaging.Event) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.inbox[event.ID] {
		return nil
	}
	store.inbox[event.ID] = true
	order := store.orders[event.Subject]
	switch event.Type {
	case "payment.failed.v1":
		if order.Status != "pending" || order.PaymentStatus != "pending" {
			return nil
		}
		order.Status, order.PaymentStatus, order.SagaState = "cancelled", "failed", "failed"
		store.events = append(store.events, derivedTestEvent("inventory.release-requested.v1", event))
	case "payment.captured.v1":
		if order.Status != "pending" || order.PaymentStatus != "pending" {
			return nil
		}
		order.Status, order.PaymentStatus, order.SagaState = "confirmed", "paid", "completed"
		store.events = append(store.events,
			derivedTestEvent("inventory.commit-requested.v1", event),
			derivedTestEvent("order.paid.v1", event))
	}
	store.orders[event.Subject] = order
	return nil
}

func derivedTestEvent(kind string, cause messaging.Event) messaging.Event {
	return messaging.Event{
		ID: kind + "-id", Type: kind, Subject: cause.Subject, SchemaVersion: 1,
		CorrelationID: cause.CorrelationID, CausationID: cause.ID, Data: json.RawMessage(`{}`),
	}
}

type customerStub struct {
	snapshot        CustomerSnapshot
	calls           int
	lastPropagation Propagation
}

func (stub *customerStub) Validate(_ context.Context, _, _ string, propagation Propagation) (CustomerSnapshot, error) {
	stub.calls++
	stub.lastPropagation = propagation
	return stub.snapshot, nil
}

type catalogStub struct {
	items []CatalogSnapshot
	calls int
}

func (stub *catalogStub) Snapshot(_ context.Context, _ []string, _ Propagation) ([]CatalogSnapshot, error) {
	stub.calls++
	return stub.items, nil
}

type inventoryStub struct {
	keys               map[string]string
	calls              int
	uniqueReservations int
	lastOrderID        string
}

func (stub *inventoryStub) Reserve(_ context.Context, command ReservationCommand, _ Propagation) error {
	stub.calls++
	payload := command.OrderID
	for _, line := range command.Lines {
		payload += line.SKU + string(rune(line.Quantity))
	}
	if prior, ok := stub.keys[command.IdempotencyKey]; ok {
		if prior != payload {
			return ErrIdempotencyConflict
		}
		return nil
	}
	stub.keys[command.IdempotencyKey] = payload
	stub.uniqueReservations++
	stub.lastOrderID = command.OrderID
	return nil
}

func (stub *inventoryStub) Release(context.Context, string, Propagation) error { return nil }

func assertEventCount(t *testing.T, events []messaging.Event, kind string, want int) {
	t.Helper()
	count := 0
	for _, event := range events {
		if event.Type == kind {
			count++
		}
	}
	if count != want {
		t.Fatalf("%s count=%d, want %d; events=%+v", kind, count, want, events)
	}
}

func assertEventCausation(t *testing.T, events []messaging.Event, kind, cause string) {
	t.Helper()
	for _, event := range events {
		if event.Type == kind {
			if event.CausationID != cause {
				t.Fatalf("%s causation=%q, want %q", kind, event.CausationID, cause)
			}
			return
		}
	}
	t.Fatalf("%s event not found", kind)
}
