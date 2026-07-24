// Package fulfillment owns fulfillment-order lifecycle rules, shipment
// projection, and the event workflow that order (Task 6) consumes.
package fulfillment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"commerce/internal/platform/messaging"
)

// Event type constants this package produces. They match order's bindings
// exactly (see cmd/order/main.go:orderEventBindings).
const (
	EventFulfillmentCompleted = "fulfillment.completed.v1"
	EventFulfillmentCancelled = "fulfillment.cancelled.v1"
)

// FulfillmentOrderStatus enumerates fulfillment_order.status values (see
// db/migrations/fulfillment_db.sql).
type FulfillmentOrderStatus string

const (
	StatusOpen       FulfillmentOrderStatus = "open"
	StatusInProgress FulfillmentOrderStatus = "in_progress"
	StatusOnHold     FulfillmentOrderStatus = "on_hold"
	StatusFulfilled  FulfillmentOrderStatus = "fulfilled"
	StatusCancelled  FulfillmentOrderStatus = "cancelled"
)

// ShipmentStatus enumerates shipment.status values.
type ShipmentStatus string

const (
	ShipmentLabelCreated ShipmentStatus = "label_created"
	ShipmentInTransit    ShipmentStatus = "in_transit"
	ShipmentDelivered    ShipmentStatus = "delivered"
	ShipmentDelayed      ShipmentStatus = "delayed"
	ShipmentException    ShipmentStatus = "exception"
	ShipmentReturned     ShipmentStatus = "returned"
	ShipmentLost         ShipmentStatus = "lost"
)

// PickStatus enumerates pick_item.status values.
type PickStatus string

const (
	PickPending  PickStatus = "pending"
	PickPicking  PickStatus = "picking"
	PickPicked   PickStatus = "picked"
	PickShort    PickStatus = "short"
	PickCanceled PickStatus = "cancelled"
)

var (
	// ErrInvalidCommand flags a malformed command (missing order/idempotency).
	ErrInvalidCommand = errors.New("invalid fulfillment command")
	// ErrNoReservations flags an order.paid.v1 with zero inventory
	// allocations (cannot be fulfilled). Non-retryable: re-delivery cannot fix
	// the upstream state.
	ErrNoReservations = errors.New("no reservations for order")
	// ErrUpstreamUnavailable flags an inventory call failure (retryable).
	ErrUpstreamUnavailable = errors.New("fulfillment upstream unavailable")
	// ErrFulfillmentNotFound flags a missing fulfillment projection.
	ErrFulfillmentNotFound = errors.New("fulfillment order not found")
)

// FulfillmentItem is a single order line routed to a fulfillment location.
type FulfillmentItem struct {
	FulfillmentID string `json:"fulfillment_id"`
	OrderItemID   string `json:"order_item_id"`
	SKU           string `json:"sku"`
	Quantity      int    `json:"quantity"`
}

// PickItem is a pick task for one fulfillment item.
type PickItem struct {
	PickItemID        string    `json:"pick_item_id"`
	FulfillmentID     string    `json:"fulfillment_id"`
	OrderItemID       string    `json:"order_item_id"`
	RequestedQuantity int       `json:"requested_quantity"`
	PickedQuantity    int       `json:"picked_quantity"`
	Status            PickStatus `json:"status"`
}

// PackageDimension is one physical package holding one or more items.
type PackageDimension struct {
	PackageID     string `json:"package_id"`
	FulfillmentID string `json:"fulfillment_id"`
	WeightGrams   int    `json:"weight_grams"`
	LengthMM      int    `json:"length_mm"`
	WidthMM       int    `json:"width_mm"`
	HeightMM      int    `json:"height_mm"`
}

// PackageItem is a single line inside a package.
type PackageItem struct {
	PackageID     string `json:"package_id"`
	FulfillmentID string `json:"fulfillment_id"`
	OrderItemID   string `json:"order_item_id"`
	Quantity      int    `json:"quantity"`
}

// TrackingEvent is a single carrier scan.
type TrackingEvent struct {
	TrackingEventID string         `json:"tracking_event_id"`
	ShipmentID      string         `json:"shipment_id"`
	Status          ShipmentStatus `json:"status"`
	Description     string         `json:"description"`
	Location        string         `json:"location,omitempty"`
	OccurredAt      time.Time      `json:"occurred_at"`
}

// Shipment is the carrier shipment for a fulfillment order.
type Shipment struct {
	ShipmentID     string           `json:"shipment_id"`
	FulfillmentID  string           `json:"fulfillment_id"`
	Carrier        string           `json:"carrier"`
	TrackingNumber string           `json:"tracking_number"`
	Status         ShipmentStatus   `json:"status"`
	ShippedAt      *time.Time       `json:"shipped_at,omitempty"`
	DeliveredAt    *time.Time       `json:"delivered_at,omitempty"`
	Packages       []PackageDimension `json:"packages,omitempty"`
	TrackingEvents []TrackingEvent  `json:"tracking_events,omitempty"`
}

// FulfillmentOrder is the per-location projection of an order's reserved lines.
type FulfillmentOrder struct {
	FulfillmentID string                 `json:"fulfillment_id"`
	OrderID       string                 `json:"order_id"`
	LocationID    string                 `json:"location_id"`
	Status        FulfillmentOrderStatus `json:"status"`
	CreatedAt     time.Time              `json:"created_at"`
	Items         []FulfillmentItem      `json:"items,omitempty"`
	Picks         []PickItem             `json:"picks,omitempty"`
	Packages      []PackageDimension     `json:"packages,omitempty"`
	Shipment      *Shipment              `json:"shipment,omitempty"`
}

func (orders FulfillmentOrderList) findByLocation(locationID string) *FulfillmentOrder {
	for index := range orders {
		if orders[index].LocationID == locationID {
			return &orders[index]
		}
	}
	return nil
}

// FulfillmentOrderList is the ordered list returned by the service.
type FulfillmentOrderList []FulfillmentOrder

// Allocation is the inventory allocation shape for a reserved line. It mirrors
// the inventory service's GET /api/v1/reservations/{order_id} payload (sku,
// location_id, quantity).
type Allocation struct {
	SKU        string `json:"sku"`
	LocationID string `json:"location_id"`
	Quantity   int64  `json:"quantity"`
}

type allocationList []Allocation

// ReservationResult mirrors the inventory HTTP response shape (order_id +
// allocations). The fulfillment service does not consume the reservation state.
type ReservationResult struct {
	OrderID     string       `json:"order_id"`
	Allocations allocationList `json:"allocations"`
}

// InventoryReservationClient fetches reserved allocations for an order.
type InventoryReservationClient interface {
	ListReservations(ctx context.Context, orderID string) (ReservationResult, error)
}

// FulfillCommand is the input produced by consuming order.paid.v1.
type FulfillCommand struct {
	OrderID        string
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
	OccurredAt     time.Time
}

// CancelCommand is the input produced by consuming order.cancelled.v1 and
// fulfillment.cancel-requested.v1.
type CancelCommand struct {
	OrderID        string
	Reason         string
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
	OccurredAt     time.Time
}

// FulfillOutcome is the in-memory result handed to the store for atomic write.
type FulfillOutcome struct {
	IdempotencyKey    string
	OrderID           string
	FulfillmentOrders FulfillmentOrderList
	Events            []messaging.Event
}

// FulfillResult is what the service returns to the caller (HTTP or consumer).
type FulfillResult struct {
	FulfillmentOrders FulfillmentOrderList
	Events            []messaging.Event
	Replayed          bool
}

// CancelOutcome is the in-memory result handed to the store for atomic write.
type CancelOutcome struct {
	IdempotencyKey    string
	OrderID           string
	Reason            string
	FulfillmentOrders FulfillmentOrderList
	Events            []messaging.Event
}

// CancelResult is what the service returns to the caller.
type CancelResult struct {
	FulfillmentOrders FulfillmentOrderList
	Events            []messaging.Event
	Replayed          bool
}

// Store persists fulfillment state and the derived Outbox events in one
// transaction. Implementations MUST make replay idempotent on idempotency_key
// and MUST NOT insert duplicate Outbox rows.
type Store interface {
	FindFulfillment(ctx context.Context, idempotencyKey string) (FulfillOutcome, bool, error)
	SaveFulfill(ctx context.Context, outcome FulfillOutcome) (FulfillResult, error)
	SaveCancel(ctx context.Context, outcome CancelOutcome) (CancelResult, error)
	GetFulfillmentsByOrder(ctx context.Context, orderID string) ([]FulfillmentOrder, error)
}

// ServiceOptions carries optional collaborators (clock).
type ServiceOptions struct {
	Clock func() time.Time
}

// Service applies fulfillment commands against the reserved-order allocation
// snapshot and persists the per-location fulfillment projection plus the
// derived Outbox event atomically.
type Service struct {
	store     Store
	inventory InventoryReservationClient
	clock     func() time.Time
}

// NewService constructs a Service bound to store and the inventory allocation
// client.
func NewService(store Store, inventory InventoryReservationClient, options ServiceOptions) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{store: store, inventory: inventory, clock: clock}
}

// FulfillOrder splits the order's reserved lines by location, creates one
// fulfillment_order per location with picks/packages/shipment, and emits
// fulfillment.completed.v1. Replays of the same idempotency key return the
// original projection without re-fetching reservations or re-emitting events.
func (service *Service) FulfillOrder(ctx context.Context, command FulfillCommand) (FulfillResult, error) {
	if service == nil || service.store == nil {
		return FulfillResult{}, errors.New("fulfillment service is unavailable")
	}
	command = canonicalFulfillCommand(command)
	if err := validateFulfillCommand(command); err != nil {
		return FulfillResult{}, err
	}
	if existing, found, err := service.store.FindFulfillment(ctx, command.IdempotencyKey); err != nil {
		return FulfillResult{}, fmt.Errorf("find fulfillment: %w", err)
	} else if found {
		return service.replayFulfill(ctx, existing)
	}
	now := service.clock().UTC()
	if command.OccurredAt.IsZero() {
		command.OccurredAt = now
	}
	reservation, err := service.fetchReservations(ctx, command.OrderID)
	if err != nil {
		return FulfillResult{}, err
	}
	if len(reservation.Allocations) == 0 {
		return FulfillResult{}, ErrNoReservations
	}
	orders := buildFulfillmentOrders(command, reservation, now)
	completed := service.completedEvent(command, now)
	outcome := FulfillOutcome{
		IdempotencyKey: command.IdempotencyKey, OrderID: command.OrderID,
		FulfillmentOrders: orders, Events: []messaging.Event{completed},
	}
	result, err := service.store.SaveFulfill(ctx, outcome)
	if err != nil {
		return FulfillResult{}, err
	}
	if result.FulfillmentOrders == nil {
		result.FulfillmentOrders = orders
	}
	return result, nil
}

func (service *Service) replayFulfill(ctx context.Context, outcome FulfillOutcome) (FulfillResult, error) {
	orders, err := service.store.GetFulfillmentsByOrder(ctx, outcome.OrderID)
	if err != nil {
		return FulfillResult{}, fmt.Errorf("load replayed fulfillment orders: %w", err)
	}
	events := make([]messaging.Event, 0)
	for _, event := range outcome.Events {
		if event.Type == EventFulfillmentCompleted && event.Subject == outcome.OrderID {
			events = append(events, event)
		}
	}
	return FulfillResult{
		FulfillmentOrders: FulfillmentOrderList(orders), Events: events, Replayed: true,
	}, nil
}

// CancelOrder cancels every still-open fulfillment order for the order and
// emits fulfillment.cancelled.v1 once. The cancelled event is emitted whenever
// the order has any prior fulfillment projection (even one already delivered),
// because the order saga needs the acknowledgement to advance — it cannot tell
// the shipments were already terminal. A duplicate cancel is an idempotent
// replay that emits no new events. Cancelling an order with no prior
// fulfillment at all is a no-op replay (the inbox delivery must still succeed).
func (service *Service) CancelOrder(ctx context.Context, command CancelCommand) (CancelResult, error) {
	if service == nil || service.store == nil {
		return CancelResult{}, errors.New("fulfillment service is unavailable")
	}
	command = canonicalCancelCommand(command)
	if err := validateCancelCommand(command); err != nil {
		return CancelResult{}, err
	}
	orders, err := service.store.GetFulfillmentsByOrder(ctx, command.OrderID)
	if err != nil {
		return CancelResult{}, fmt.Errorf("list fulfillment orders for cancel: %w", err)
	}
	if len(orders) == 0 {
		// No prior fulfillment projection. Record the idempotency key via a
		// no-op SaveCancel so a duplicate delivery converges without emitting
		// an event the order saga cannot pair with a fulfillment.
		noop := CancelOutcome{
			IdempotencyKey: command.IdempotencyKey, OrderID: command.OrderID,
			Reason: command.Reason,
		}
		noopResult, err := service.store.SaveCancel(ctx, noop)
		if err != nil {
			return CancelResult{}, err
		}
		noopResult.Replayed = true
		return noopResult, nil
	}
	now := service.clock().UTC()
	if command.OccurredAt.IsZero() {
		command.OccurredAt = now
	}
	cancelled := make(FulfillmentOrderList, 0, len(orders))
	for _, order := range orders {
		if order.Status == StatusOpen || order.Status == StatusInProgress ||
			order.Status == StatusOnHold {
			order.Status = StatusCancelled
		}
		cancelled = append(cancelled, order)
	}
	event := service.cancelledEvent(command, now)
	outcome := CancelOutcome{
		IdempotencyKey: command.IdempotencyKey, OrderID: command.OrderID,
		Reason: command.Reason, FulfillmentOrders: cancelled,
		Events: []messaging.Event{event},
	}
	return service.store.SaveCancel(ctx, outcome)
}

func (service *Service) fetchReservations(ctx context.Context, orderID string) (ReservationResult, error) {
	if service.inventory == nil {
		return ReservationResult{}, fmt.Errorf("%w: inventory client is unavailable", ErrUpstreamUnavailable)
	}
	result, err := service.inventory.ListReservations(ctx, orderID)
	if err != nil {
		return ReservationResult{}, fmt.Errorf("%w: fetch reservations: %v", ErrUpstreamUnavailable, err)
	}
	if result.OrderID == "" {
		result.OrderID = orderID
	}
	return result, nil
}

// buildFulfillmentOrders groups the reserved allocations by location_id and
// constructs the deterministic per-location projection (items, picks, packages,
// shipment, tracking events). The projection advances the shipment to
// `delivered` synchronously so the domain event is emitted in the same
// transaction (Task 7 contract).
func buildFulfillmentOrders(
	command FulfillCommand,
	reservation ReservationResult,
	now time.Time,
) FulfillmentOrderList {
	groups := groupAllocationsByLocation(reservation.Allocations)
	orders := make(FulfillmentOrderList, 0, len(groups))
	for _, group := range groups {
		locationID := group.locationID
		fulfillmentID := deterministicFulfillmentID(command.OrderID, locationID)
		items := make([]FulfillmentItem, 0, len(group.allocations))
		picks := make([]PickItem, 0, len(group.allocations))
		for _, allocation := range group.allocations {
			orderItemID := deterministicOrderItemID(command.OrderID, allocation.SKU)
			quantity := int(allocation.Quantity)
			items = append(items, FulfillmentItem{
				FulfillmentID: fulfillmentID, OrderItemID: orderItemID,
				SKU: allocation.SKU, Quantity: quantity,
			})
			picks = append(picks, PickItem{
				PickItemID:        deterministicPickItemID(fulfillmentID, orderItemID),
				FulfillmentID:     fulfillmentID, OrderItemID: orderItemID,
				RequestedQuantity: quantity, PickedQuantity: quantity, Status: PickPicked,
			})
		}
		packages := buildPackages(fulfillmentID, items)
		shipment := buildShipment(command.OrderID, fulfillmentID, locationID, packages, now)
		orders = append(orders, FulfillmentOrder{
			FulfillmentID: fulfillmentID, OrderID: command.OrderID, LocationID: locationID,
			Status: StatusFulfilled, CreatedAt: now, Items: items, Picks: picks,
			Packages: packages, Shipment: shipment,
		})
	}
	sort.Slice(orders, func(i, j int) bool {
		return orders[i].LocationID < orders[j].LocationID
	})
	return orders
}

type allocationGroup struct {
	locationID string
	allocations allocationList
}

func groupAllocationsByLocation(allocations allocationList) []allocationGroup {
	index := make(map[string]int)
	groups := make([]allocationGroup, 0)
	for _, allocation := range allocations {
		position, seen := index[allocation.LocationID]
		if !seen {
			groups = append(groups, allocationGroup{locationID: allocation.LocationID})
			position = len(groups) - 1
			index[allocation.LocationID] = position
		}
		groups[position].allocations = append(groups[position].allocations, allocation)
	}
	for position := range groups {
		sort.Slice(groups[position].allocations, func(i, j int) bool {
			return groups[position].allocations[i].SKU < groups[position].allocations[j].SKU
		})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].locationID < groups[j].locationID
	})
	return groups
}

// buildPackages packs every item into a single package per fulfillment order.
// The simulated carrier accepts one parcel per warehouse pick run for the
// default scenario; the schema permits multi-package shipments so a richer
// packing rule can replace this without changing the public contract.
func buildPackages(fulfillmentID string, items []FulfillmentItem) []PackageDimension {
	if len(items) == 0 {
		return nil
	}
	packageID := deterministicPackageID(fulfillmentID, 1)
	return []PackageDimension{{
		PackageID: packageID, FulfillmentID: fulfillmentID,
		WeightGrams: defaultPackageWeightGrams, LengthMM: defaultPackageLengthMM,
		WidthMM: defaultPackageWidthMM, HeightMM: defaultPackageHeightMM,
	}}
}

func buildShipment(orderID, fulfillmentID, locationID string, packages []PackageDimension, now time.Time) *Shipment {
	carrier, tracking := deterministicCarrierTracking(orderID, locationID)
	shipmentID := deterministicShipmentID(fulfillmentID)
	shippedAt := now
	deliveredAt := now
	trackingEvents := []TrackingEvent{
		{
			TrackingEventID: deterministicTrackingEventID(shipmentID, 1),
			ShipmentID: shipmentID, Status: ShipmentLabelCreated,
			Description: "label created", Location: locationID,
			OccurredAt: now,
		},
		{
			TrackingEventID: deterministicTrackingEventID(shipmentID, 2),
			ShipmentID: shipmentID, Status: ShipmentInTransit,
			Description: "in transit", Location: carrierHub(carrier),
			OccurredAt: now,
		},
		{
			TrackingEventID: deterministicTrackingEventID(shipmentID, 3),
			ShipmentID: shipmentID, Status: ShipmentDelivered,
			Description: "delivered", Location: locationID,
			OccurredAt: now,
		},
	}
	return &Shipment{
		ShipmentID: shipmentID, FulfillmentID: fulfillmentID,
		Carrier: carrier, TrackingNumber: tracking,
		Status: ShipmentDelivered, ShippedAt: &shippedAt, DeliveredAt: &deliveredAt,
		Packages: packages, TrackingEvents: trackingEvents,
	}
}

const (
	defaultPackageWeightGrams = 500
	defaultPackageLengthMM    = 200
	defaultPackageWidthMM     = 150
	defaultPackageHeightMM    = 50
)

// carriers is the deterministic carrier table. The selected carrier depends
// only on a stable hash of (order_id, location_id), never on wall-clock time.
var carriers = []string{"SpeedyExpress", "GlobeFreight", "PolarLogistics"}

func carrierHub(carrier string) string {
	switch carrier {
	case "SpeedyExpress":
		return "SHENZHEN_HUB"
	case "GlobeFreight":
		return "SHANGHAI_HUB"
	case "PolarLogistics":
		return "BEIJING_HUB"
	default:
		return "FULFILLMENT_HUB"
	}
}

func deterministicCarrierTracking(orderID, locationID string) (carrier, tracking string) {
	sum := sha256.Sum256([]byte("fulfillment_carrier\x00" + orderID + "\x00" + locationID))
	carrier = carriers[int(sum[0])%len(carriers)]
	tracking = "TRK" + hex.EncodeToString(sum[:12])
	return carrier, tracking
}

// --- event construction ---

// orderIDPayload is the EXACT shape order decodes with DisallowUnknownFields.
// Do NOT add fields here without updating the order saga's contract table.
type orderIDPayload struct {
	OrderID string `json:"order_id"`
}

func (service *Service) completedEvent(command FulfillCommand, now time.Time) messaging.Event {
	body, _ := json.Marshal(orderIDPayload{OrderID: command.OrderID})
	return messaging.NewEvent(EventFulfillmentCompleted, command.OrderID,
		command.CorrelationID, command.CausationID, body, func() time.Time { return now })
}

func (service *Service) cancelledEvent(command CancelCommand, now time.Time) messaging.Event {
	body, _ := json.Marshal(orderIDPayload{OrderID: command.OrderID})
	return messaging.NewEvent(EventFulfillmentCancelled, command.OrderID,
		command.CorrelationID, command.CausationID, body, func() time.Time { return now })
}

// --- validation / canonicalisation ---

func canonicalFulfillCommand(command FulfillCommand) FulfillCommand {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	command.CausationID = strings.TrimSpace(command.CausationID)
	return command
}

func validateFulfillCommand(command FulfillCommand) error {
	if command.OrderID == "" || command.IdempotencyKey == "" {
		return fmt.Errorf("%w: order/idempotency required", ErrInvalidCommand)
	}
	return nil
}

func canonicalCancelCommand(command CancelCommand) CancelCommand {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.Reason = strings.TrimSpace(command.Reason)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	command.CausationID = strings.TrimSpace(command.CausationID)
	return command
}

func validateCancelCommand(command CancelCommand) error {
	if command.OrderID == "" || command.Reason == "" || command.IdempotencyKey == "" {
		return fmt.Errorf("%w: order/reason/idempotency required", ErrInvalidCommand)
	}
	return nil
}

// --- deterministic IDs ---

func deterministicFulfillmentID(orderID, locationID string) string {
	sum := sha256.Sum256([]byte("fulfillment_order\x00" + orderID + "\x00" + locationID))
	return "ful_" + hex.EncodeToString(sum[:12])
}

func deterministicOrderItemID(orderID, sku string) string {
	sum := sha256.Sum256([]byte("fulfillment_order_item\x00" + orderID + "\x00" + sku))
	return "oit_" + hex.EncodeToString(sum[:12])
}

func deterministicPickItemID(fulfillmentID, orderItemID string) string {
	sum := sha256.Sum256([]byte("fulfillment_pick_item\x00" + fulfillmentID + "\x00" + orderItemID))
	return "pic_" + hex.EncodeToString(sum[:12])
}

func deterministicPackageID(fulfillmentID string, sequence int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fulfillment_package\x00%s\x00%d", fulfillmentID, sequence)))
	return "pkg_" + hex.EncodeToString(sum[:12])
}

func deterministicShipmentID(fulfillmentID string) string {
	sum := sha256.Sum256([]byte("fulfillment_shipment\x00" + fulfillmentID))
	return "shp_" + hex.EncodeToString(sum[:12])
}

func deterministicTrackingEventID(shipmentID string, sequence int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fulfillment_tracking\x00%s\x00%d", shipmentID, sequence)))
	return "trk_" + hex.EncodeToString(sum[:12])
}
