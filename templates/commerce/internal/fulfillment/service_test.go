package fulfillment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"commerce/internal/platform/messaging"
)

func fixedFulfillmentClock() time.Time {
	return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
}

func TestPaidOrderSplitsFulfillmentByLocation(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-1", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 2},
		{SKU: "SKU-B", LocationID: "LOC-2", Quantity: 1},
	})
	result, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-1", IdempotencyKey: "paid-1", CorrelationID: "request-1",
	})
	if err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	if len(result.FulfillmentOrders) != 2 {
		t.Fatalf("fulfillment orders=%d, want 2 (one per location)", len(result.FulfillmentOrders))
	}
	if result.FulfillmentOrders[0].LocationID == result.FulfillmentOrders[1].LocationID {
		t.Fatalf("duplicate location_id=%q", result.FulfillmentOrders[0].LocationID)
	}
	if len(result.Events) != 1 || result.Events[0].Type != EventFulfillmentCompleted {
		t.Fatalf("events=%+v, want single %s", eventTypes(result.Events), EventFulfillmentCompleted)
	}
	if result.Replayed {
		t.Fatalf("first fulfillment must not be a replay")
	}
}

func TestFulfillOrderGroupsMultipleSKUsPerLocation(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-2", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 2},
		{SKU: "SKU-B", LocationID: "LOC-1", Quantity: 3},
		{SKU: "SKU-C", LocationID: "LOC-2", Quantity: 1},
	})
	result, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-2", IdempotencyKey: "paid-2", CorrelationID: "request-2",
	})
	if err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	if len(result.FulfillmentOrders) != 2 {
		t.Fatalf("fulfillment orders=%d, want 2", len(result.FulfillmentOrders))
	}
	loc1 := result.FulfillmentOrders.findByLocation("LOC-1")
	if loc1 == nil {
		t.Fatalf("missing LOC-1 fulfillment order")
	}
	if len(loc1.Items) != 2 {
		t.Fatalf("LOC-1 items=%d, want 2 (SKU-A + SKU-B)", len(loc1.Items))
	}
}

func TestFulfillOrderCreatesPackagesAndShipmentPerLocation(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-3", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 2},
	})
	result, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-3", IdempotencyKey: "paid-3", CorrelationID: "request-3",
	})
	if err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	if len(result.FulfillmentOrders) != 1 {
		t.Fatalf("fulfillment orders=%d, want 1", len(result.FulfillmentOrders))
	}
	order := result.FulfillmentOrders[0]
	if order.Status != StatusFulfilled {
		t.Fatalf("status=%q, want %q", order.Status, StatusFulfilled)
	}
	if len(order.Packages) == 0 {
		t.Fatalf("expected at least one package")
	}
	if order.Shipment == nil {
		t.Fatalf("expected shipment projection")
	}
	if order.Shipment.Carrier == "" || order.Shipment.TrackingNumber == "" {
		t.Fatalf("shipment missing carrier/tracking: %+v", order.Shipment)
	}
	if order.Shipment.Status != ShipmentDelivered {
		t.Fatalf("shipment status=%q, want %q (projection advances to delivered)",
			order.Shipment.Status, ShipmentDelivered)
	}
	if order.Shipment.TrackingEvents == nil || len(order.Shipment.TrackingEvents) == 0 {
		t.Fatalf("expected tracking events")
	}
}

func TestFulfillOrderCarrierAndTrackingAreDeterministic(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-4", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
	})
	first, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-4", IdempotencyKey: "paid-4", CorrelationID: "request-4",
	})
	if err != nil {
		t.Fatalf("first FulfillOrder error: %v", err)
	}
	fixture2 := newFulfillmentFixture()
	fixture2.inventory.seed("ORD-4", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
	})
	second, err := fixture2.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-4", IdempotencyKey: "paid-4-alt", CorrelationID: "request-4",
	})
	if err != nil {
		t.Fatalf("second FulfillOrder error: %v", err)
	}
	if first.FulfillmentOrders[0].Shipment.Carrier != second.FulfillmentOrders[0].Shipment.Carrier {
		t.Fatalf("carrier differs across runs: %q vs %q",
			first.FulfillmentOrders[0].Shipment.Carrier,
			second.FulfillmentOrders[0].Shipment.Carrier)
	}
	if first.FulfillmentOrders[0].Shipment.TrackingNumber !=
		second.FulfillmentOrders[0].Shipment.TrackingNumber {
		t.Fatalf("tracking differs across runs: %q vs %q",
			first.FulfillmentOrders[0].Shipment.TrackingNumber,
			second.FulfillmentOrders[0].Shipment.TrackingNumber)
	}
}

func TestDuplicateOrderPaidReplayIsIdempotent(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-5", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
		{SKU: "SKU-B", LocationID: "LOC-2", Quantity: 1},
	})
	command := FulfillCommand{
		OrderID: "ORD-5", IdempotencyKey: "paid-5", CorrelationID: "request-5",
	}
	first, err := fixture.service.FulfillOrder(context.Background(), command)
	if err != nil {
		t.Fatalf("first FulfillOrder error: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first fulfillment must not be a replay")
	}
	if fixture.inventory.calls != 1 {
		t.Fatalf("inventory calls=%d, want 1", fixture.inventory.calls)
	}
	second, err := fixture.service.FulfillOrder(context.Background(), command)
	if err != nil {
		t.Fatalf("second FulfillOrder error: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("second fulfillment must be a replay")
	}
	if fixture.inventory.calls != 1 {
		t.Fatalf("inventory calls after replay=%d, want 1 (no re-fetch on replay)",
			fixture.inventory.calls)
	}
	completed := 0
	for _, event := range fixture.store.capturedEvents {
		if event.Type == EventFulfillmentCompleted && event.Subject == "ORD-5" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("completed events=%d, want 1 (no duplicates on replay)", completed)
	}
}

func TestCompletedEventPayloadMatchesOrderContractExactly(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-6", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
	})
	result, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-6", IdempotencyKey: "paid-6", CorrelationID: "request-6",
		CausationID: "cause-6",
	})
	if err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	event := result.Events[0]
	var payload orderIDPayload
	if err := decodeStrict(event.Data, &payload); err != nil {
		t.Fatalf("decode completed payload: %v", err)
	}
	if payload.OrderID != "ORD-6" {
		t.Fatalf("payload order_id=%q, want ORD-6", payload.OrderID)
	}
	if event.Subject != "ORD-6" {
		t.Fatalf("subject=%q, want ORD-6", event.Subject)
	}
	if event.SchemaVersion != messaging.CurrentSchemaVersion {
		t.Fatalf("schema_version=%d, want %d", event.SchemaVersion, messaging.CurrentSchemaVersion)
	}
	if event.OccurredAt.IsZero() {
		t.Fatalf("occurred_at must be non-zero")
	}
	if event.CorrelationID != "request-6" {
		t.Fatalf("correlation_id=%q, want request-6", event.CorrelationID)
	}
	if event.CausationID != "cause-6" {
		t.Fatalf("causation_id=%q, want cause-6", event.CausationID)
	}
	// The JSON Data must contain exactly one key ("order_id"). A second key
	// would break order's DisallowUnknownFields decoder.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(event.Data, &raw); err != nil {
		t.Fatalf("decode completed payload as map: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("completed payload keys=%d, want 1 (order_id only)", len(raw))
	}
	if _, ok := raw["order_id"]; !ok {
		t.Fatalf("completed payload missing order_id key; got %v", raw)
	}
}

func TestFulfillOrderRejectsInvalidCommand(t *testing.T) {
	fixture := newFulfillmentFixture()
	for _, command := range []FulfillCommand{
		{OrderID: "", IdempotencyKey: "k"},
		{OrderID: "ORD", IdempotencyKey: ""},
	} {
		if _, err := fixture.service.FulfillOrder(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("FulfillOrder(%+v) error=%v, want ErrInvalidCommand", command, err)
		}
	}
}

func TestFulfillOrderFailsWhenInventoryHasNoReservations(t *testing.T) {
	fixture := newFulfillmentFixture()
	// No allocations seeded — simulate an order that was never reserved.
	if _, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-7", IdempotencyKey: "paid-7", CorrelationID: "request-7",
	}); !errors.Is(err, ErrNoReservations) {
		t.Fatalf("FulfillOrder error=%v, want ErrNoReservations", err)
	}
}

func TestFulfillOrderFailsWhenInventoryClientErrors(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.fail(errors.New("inventory offline"))
	if _, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-8", IdempotencyKey: "paid-8", CorrelationID: "request-8",
	}); !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("FulfillOrder error=%v, want ErrUpstreamUnavailable", err)
	}
}

func TestCancelAfterFulfillmentEmitsCancelledEvent(t *testing.T) {
	// The order saga emits fulfillment.cancel-requested.v1 even for an order
	// whose shipments are already delivered. Fulfillment must acknowledge with
	// fulfillment.cancelled.v1 so the saga can complete its compensation; the
	// already-delivered projection itself is terminal and stays as-is.
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-9", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
		{SKU: "SKU-B", LocationID: "LOC-2", Quantity: 1},
	})
	if _, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-9", IdempotencyKey: "paid-9", CorrelationID: "request-9",
	}); err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	fixture.store.resetEvents()
	result, err := fixture.service.CancelOrder(context.Background(), CancelCommand{
		OrderID: "ORD-9", Reason: "buyer_cancelled",
		IdempotencyKey: "cancel-9", CorrelationID: "request-9",
	})
	if err != nil {
		t.Fatalf("CancelOrder error: %v", err)
	}
	if len(result.FulfillmentOrders) != 2 {
		t.Fatalf("returned orders=%d, want 2", len(result.FulfillmentOrders))
	}
	if len(result.Events) != 1 || result.Events[0].Type != EventFulfillmentCancelled {
		t.Fatalf("events=%+v, want single %s", eventTypes(result.Events), EventFulfillmentCancelled)
	}
	var payload orderIDPayload
	if err := decodeStrict(result.Events[0].Data, &payload); err != nil {
		t.Fatalf("decode cancelled payload: %v", err)
	}
	if payload.OrderID != "ORD-9" {
		t.Fatalf("payload order_id=%q, want ORD-9", payload.OrderID)
	}
	if result.Events[0].Subject != "ORD-9" {
		t.Fatalf("subject=%q, want ORD-9", result.Events[0].Subject)
	}
}

func TestCancelOpenFulfillmentFlipsStatusToCancelled(t *testing.T) {
	// Seed a non-terminal fulfillment directly so the cancel path can flip
	// status to cancelled (the terminal-delivered case is covered above).
	fixture := newFulfillmentFixture()
	open := FulfillmentOrder{
		FulfillmentID: deterministicFulfillmentID("ORD-9B", "LOC-1"),
		OrderID: "ORD-9B", LocationID: "LOC-1", Status: StatusInProgress,
		CreatedAt: fixedFulfillmentClock(),
	}
	fixture.store.seedOrder(open)
	result, err := fixture.service.CancelOrder(context.Background(), CancelCommand{
		OrderID: "ORD-9B", Reason: "buyer_cancelled",
		IdempotencyKey: "cancel-9b", CorrelationID: "request-9b",
	})
	if err != nil {
		t.Fatalf("CancelOrder error: %v", err)
	}
	if len(result.FulfillmentOrders) != 1 {
		t.Fatalf("orders=%d, want 1", len(result.FulfillmentOrders))
	}
	if result.FulfillmentOrders[0].Status != StatusCancelled {
		t.Fatalf("status=%q, want %q", result.FulfillmentOrders[0].Status, StatusCancelled)
	}
}

func TestCancelOrderIsIdempotentForAlreadyCancelled(t *testing.T) {
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-10", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
	})
	if _, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-10", IdempotencyKey: "paid-10", CorrelationID: "request-10",
	}); err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	first, err := fixture.service.CancelOrder(context.Background(), CancelCommand{
		OrderID: "ORD-10", Reason: "first",
		IdempotencyKey: "cancel-10", CorrelationID: "request-10",
	})
	if err != nil {
		t.Fatalf("first CancelOrder error: %v", err)
	}
	if first.Replayed {
		t.Fatalf("first cancel must not be a replay")
	}
	fixture.store.resetEvents()
	second, err := fixture.service.CancelOrder(context.Background(), CancelCommand{
		OrderID: "ORD-10", Reason: "second",
		IdempotencyKey: "cancel-10", CorrelationID: "request-10",
	})
	if err != nil {
		t.Fatalf("second CancelOrder error: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("second cancel must be a replay")
	}
	cancelledEvents := 0
	for _, event := range fixture.store.capturedEvents {
		if event.Type == EventFulfillmentCancelled && event.Subject == "ORD-10" {
			cancelledEvents++
		}
	}
	if cancelledEvents != 0 {
		t.Fatalf("cancelled events after replay=%d, want 0 (no duplicates)",
			cancelledEvents)
	}
}

func TestCancelOrderForUnknownOrderIsIdempotentNoOp(t *testing.T) {
	fixture := newFulfillmentFixture()
	// No prior fulfillment exists — order.cancelled.v1 must not block the inbox.
	result, err := fixture.service.CancelOrder(context.Background(), CancelCommand{
		OrderID: "ORD-11", Reason: "x",
		IdempotencyKey: "cancel-11", CorrelationID: "request-11",
	})
	if err != nil {
		t.Fatalf("CancelOrder error: %v", err)
	}
	if len(result.Events) != 0 {
		t.Fatalf("events=%+v, want none (no prior fulfillment)", eventTypes(result.Events))
	}
	if !result.Replayed {
		t.Fatalf("cancel of unknown order must be a replay no-op")
	}
}

func TestCancelOrderRejectsInvalidCommand(t *testing.T) {
	fixture := newFulfillmentFixture()
	for _, command := range []CancelCommand{
		{OrderID: "", Reason: "x", IdempotencyKey: "k"},
		{OrderID: "ORD", Reason: "", IdempotencyKey: "k"},
		{OrderID: "ORD", Reason: "x", IdempotencyKey: ""},
	} {
		if _, err := fixture.service.CancelOrder(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("CancelOrder(%+v) error=%v, want ErrInvalidCommand", command, err)
		}
	}
}

func TestFulfillOrderEmitsSingleCompletedEventForMultipleLocations(t *testing.T) {
	// Order aggregates the per-location completions into ONE domain event so the
	// outbound payload stays exactly {order_id}. Multiple fulfillment orders still
	// mean a single shipment completion at the domain boundary.
	fixture := newFulfillmentFixture()
	fixture.inventory.seed("ORD-12", allocationList{
		{SKU: "SKU-A", LocationID: "LOC-1", Quantity: 1},
		{SKU: "SKU-B", LocationID: "LOC-2", Quantity: 1},
		{SKU: "SKU-C", LocationID: "LOC-3", Quantity: 1},
	})
	result, err := fixture.service.FulfillOrder(context.Background(), FulfillCommand{
		OrderID: "ORD-12", IdempotencyKey: "paid-12", CorrelationID: "request-12",
	})
	if err != nil {
		t.Fatalf("FulfillOrder error: %v", err)
	}
	if len(result.FulfillmentOrders) != 3 {
		t.Fatalf("fulfillment orders=%d, want 3", len(result.FulfillmentOrders))
	}
	if len(result.Events) != 1 {
		t.Fatalf("events=%d, want 1 (one completed event per order.paid.v1 delivery)",
			len(result.Events))
	}
}

// --- helpers / fakes ---

func decodeStrict(data json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(destination)
}

func eventTypes(events []messaging.Event) []string {
	types := make([]string, len(events))
	for index, event := range events {
		types[index] = event.Type
	}
	return types
}

type fulfillmentFixture struct {
	service   *Service
	store     *fakeStore
	inventory *fakeInventoryClient
}

func newFulfillmentFixture() *fulfillmentFixture {
	store := newFakeStore()
	inventory := newFakeInventoryClient()
	service := NewService(store, inventory, ServiceOptions{Clock: fixedFulfillmentClock})
	return &fulfillmentFixture{service: service, store: store, inventory: inventory}
}

type fakeInventoryClient struct {
	mu          sync.Mutex
	reservations map[string]allocationList
	failErr     error
	calls       int
}

func newFakeInventoryClient() *fakeInventoryClient {
	return &fakeInventoryClient{reservations: make(map[string]allocationList)}
}

func (client *fakeInventoryClient) seed(orderID string, allocations allocationList) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.reservations[orderID] = append(client.reservations[orderID][:0], allocations...)
}

func (client *fakeInventoryClient) fail(err error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.failErr = err
}

func (client *fakeInventoryClient) ListReservations(ctx context.Context, orderID string) (ReservationResult, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.calls++
	if client.failErr != nil {
		return ReservationResult{}, client.failErr
	}
	allocations, ok := client.reservations[orderID]
	if !ok {
		return ReservationResult{OrderID: orderID}, nil
	}
	return ReservationResult{OrderID: orderID, Allocations: append(allocationList(nil), allocations...)}, nil
}

type fakeStore struct {
	mu             sync.Mutex
	fulfillments   map[string]FulfillOutcome // idempotency_key -> outcome
	byOrder        map[string][]FulfillmentOrder
	cancelsByKey   map[string]CancelResult
	capturedEvents []messaging.Event
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		fulfillments: make(map[string]FulfillOutcome),
		byOrder:      make(map[string][]FulfillmentOrder),
		cancelsByKey: make(map[string]CancelResult),
	}
}

func (store *fakeStore) seedOrder(order FulfillmentOrder) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.byOrder[order.OrderID] = append(store.byOrder[order.OrderID][:0], order)
}

func (store *fakeStore) resetEvents() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.capturedEvents = nil
}

func (store *fakeStore) FindFulfillment(ctx context.Context, idempotencyKey string) (FulfillOutcome, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	outcome, found := store.fulfillments[idempotencyKey]
	return outcome, found, nil
}

func (store *fakeStore) SaveFulfill(ctx context.Context, outcome FulfillOutcome) (FulfillResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.fulfillments[outcome.IdempotencyKey]; exists {
		existing := store.fulfillments[outcome.IdempotencyKey]
		orders := store.byOrder[existing.OrderID]
		events := make([]messaging.Event, 0)
		for _, event := range store.capturedEvents {
			if event.Subject == existing.OrderID && event.Type == EventFulfillmentCompleted {
				events = append(events, event)
			}
		}
		return FulfillResult{
			FulfillmentOrders: cloneFulfillmentOrders(orders),
			Events:            events, Replayed: true,
		}, nil
	}
	store.fulfillments[outcome.IdempotencyKey] = outcome
	store.byOrder[outcome.OrderID] = append(store.byOrder[outcome.OrderID][:0], outcome.FulfillmentOrders...)
	store.capturedEvents = append(store.capturedEvents, outcome.Events...)
	return FulfillResult{
		FulfillmentOrders: cloneFulfillmentOrders(outcome.FulfillmentOrders),
		Events:            append([]messaging.Event(nil), outcome.Events...),
	}, nil
}

func (store *fakeStore) SaveCancel(ctx context.Context, outcome CancelOutcome) (CancelResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.cancelsByKey[outcome.IdempotencyKey]; exists {
		existing := store.cancelsByKey[outcome.IdempotencyKey]
		existing.Replayed = true
		return existing, nil
	}
	orders := store.byOrder[outcome.OrderID]
	for index := range orders {
		if orders[index].Status != StatusFulfilled {
			orders[index].Status = StatusCancelled
		}
	}
	result := CancelResult{
		FulfillmentOrders: cloneFulfillmentOrders(orders),
		Events:            append([]messaging.Event(nil), outcome.Events...),
	}
	store.cancelsByKey[outcome.IdempotencyKey] = result
	store.byOrder[outcome.OrderID] = orders
	store.capturedEvents = append(store.capturedEvents, outcome.Events...)
	return result, nil
}

func (store *fakeStore) GetFulfillmentsByOrder(ctx context.Context, orderID string) ([]FulfillmentOrder, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneFulfillmentOrders(store.byOrder[orderID]), nil
}

func cloneFulfillmentOrders(orders []FulfillmentOrder) []FulfillmentOrder {
	if orders == nil {
		return nil
	}
	cloned := make([]FulfillmentOrder, len(orders))
	for index := range orders {
		cloned[index] = orders[index]
	}
	return cloned
}

var _ Store = (*fakeStore)(nil)
var _ InventoryReservationClient = (*fakeInventoryClient)(nil)
