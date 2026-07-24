// Package order owns carts, immutable checkout snapshots, and the order saga.
package order

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"commerce/internal/platform/messaging"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

var (
	ErrCartNotFound        = errors.New("cart not found")
	ErrOrderNotFound       = errors.New("order not found")
	ErrInvalidCommand      = errors.New("invalid order command")
	ErrVersionConflict     = errors.New("cart version conflict")
	ErrIdempotencyConflict = errors.New("checkout idempotency conflict")
	ErrCheckoutUncertain   = errors.New("checkout outcome uncertain")
	ErrUpstreamUnavailable = errors.New("order upstream unavailable")
	errInvalidOrderCursor  = errors.New("invalid order cursor")
)

type CartStatus string

const (
	CartActive    CartStatus = "active"
	CartConverted CartStatus = "converted"
	CartAbandoned CartStatus = "abandoned"
	CartExpired   CartStatus = "expired"
)

type CartLine struct {
	SKU            string `json:"sku"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor,omitempty"`
}

type Cart struct {
	ID             string     `json:"cart_id"`
	CustomerID     string     `json:"customer_id"`
	Status         CartStatus `json:"status"`
	Currency       string     `json:"currency"`
	Version        int64      `json:"version"`
	ExpiresAt      time.Time  `json:"expires_at"`
	Lines          []CartLine `json:"lines"`
	IdempotencyKey string     `json:"-"`
	Replayed       bool       `json:"-"`
}

type CartMutationAction string

const (
	CartAddLine    CartMutationAction = "add"
	CartChangeLine CartMutationAction = "change"
	CartRemoveLine CartMutationAction = "remove"
)

type CartMutation struct {
	CartID          string
	SKU             string
	Quantity        int64
	ExpectedVersion int64
	Action          CartMutationAction
	IdempotencyKey  string
}

type CustomerSnapshot struct {
	ID      string          `json:"customer_id"`
	Email   string          `json:"email"`
	Name    string          `json:"name"`
	Phone   string          `json:"phone,omitempty"`
	Address json.RawMessage `json:"address"`
}

type CatalogSnapshot struct {
	ProductID      string         `json:"product_id"`
	SKU            string         `json:"sku"`
	ProductTitle   string         `json:"product_title"`
	VariantTitle   string         `json:"variant_title"`
	Title          string         `json:"title"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	WeightGrams    int            `json:"weight_grams"`
	UnitPriceMinor int64          `json:"unit_price_minor"`
	Currency       string         `json:"currency"`
	Channel        string         `json:"channel"`
}

type OrderLine struct {
	ID             string         `json:"order_item_id"`
	SKU            string         `json:"sku"`
	Title          string         `json:"title"`
	Quantity       int64          `json:"quantity"`
	UnitPriceMinor int64          `json:"unit_price_minor"`
	DiscountMinor  int64          `json:"discount_minor"`
	TotalMinor     int64          `json:"total_minor"`
	ProductID      string         `json:"product_id,omitempty"`
	ProductTitle   string         `json:"product_title,omitempty"`
	VariantTitle   string         `json:"variant_title,omitempty"`
	Attributes     map[string]any `json:"attributes,omitempty"`
	WeightGrams    int            `json:"weight_grams,omitempty"`
	Channel        string         `json:"channel,omitempty"`
	TaxMinor       int64          `json:"tax_minor,omitempty"`
	TaxRateBPS     int64          `json:"tax_rate_basis_points,omitempty"`
}

type Order struct {
	OrderID           string           `json:"order_id"`
	Number            string           `json:"order_no"`
	CustomerID        string           `json:"customer_id"`
	Status            string           `json:"status"`
	PaymentStatus     string           `json:"payment_status"`
	FulfillmentStatus string           `json:"fulfillment_status"`
	Currency          string           `json:"currency"`
	SubtotalMinor     int64            `json:"subtotal_minor"`
	DiscountMinor     int64            `json:"discount_minor"`
	ShippingMinor     int64            `json:"shipping_minor"`
	TaxMinor          int64            `json:"tax_minor"`
	TotalMinor        int64            `json:"total_minor"`
	Customer          CustomerSnapshot `json:"customer"`
	ShippingAddress   json.RawMessage  `json:"shipping_address"`
	Lines             []OrderLine      `json:"lines"`
	IdempotencyKey    string           `json:"-"`
	PlacedAt          time.Time        `json:"placed_at"`
	SagaState         string           `json:"saga_state,omitempty"`
	CouponCode        string           `json:"coupon_code,omitempty"`
	Replayed          bool             `json:"-"`
}

type OrderCursor struct {
	PlacedAt time.Time `json:"placed_at"`
	OrderID  string    `json:"order_id"`
}

type OrderPage struct {
	Items      []Order `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type CheckoutCommand struct {
	CartID         string
	AddressID      string
	IdempotencyKey string
	RequestID      string
	Traceparent    string
	CorrelationID  string
	CouponCode     string
}

type CreateCartCommand struct {
	CustomerID     string
	Currency       string
	IdempotencyKey string
}

type CancelCommand struct {
	OrderID        string
	Reason         string
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
}

type CheckoutRecord struct {
	RequestHash         string
	Order               Order
	Phase               string
	PreparedOrder       Order
	PreparedReservation ReservationCommand
	ReservationResult   ReservationResult
}

type CheckoutClaim struct {
	IdempotencyKey string
	RequestHash    string
	Cart           Cart
	OrderID        string
	Now            time.Time
}

type CheckoutCommit struct {
	CartID      string
	CartVersion int64
	RequestHash string
	Order       Order
	Event       messaging.Event
}

type ReservationLine struct {
	SKU      string `json:"sku"`
	Quantity int64  `json:"quantity"`
}

type ReservationCommand struct {
	OrderID        string            `json:"order_id"`
	IdempotencyKey string            `json:"-"`
	Lines          []ReservationLine `json:"lines"`
}

// ReservationAllocation mirrors inventory's 7-field allocation shape so the
// order saga can decode real inventory.committed.v1/inventory.released.v1
// events under DisallowUnknownFields. Order only consumes SKU/Quantity/Status
// (see validateInventoryResult); OrderID/LocationID/ExpiresAt are accepted to
// stay in lock-step with the producer contract in internal/inventory/service.go
// and would otherwise reject every real event. Keep the JSON tags identical to
// inventory.ReservationAllocation.
type ReservationAllocation struct {
	AllocationID string    `json:"reservation_id"`
	OrderID      string    `json:"order_id"`
	SKU          string    `json:"sku"`
	LocationID   string    `json:"location_id"`
	Quantity     int64     `json:"quantity"`
	Status       string    `json:"status"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type ReservationResult struct {
	OrderID     string                  `json:"order_id"`
	Allocations []ReservationAllocation `json:"allocations"`
}

type Propagation struct {
	RequestID      string
	Traceparent    string
	IdempotencyKey string
	CorrelationID  string
	CausationID    string
}

type Store interface {
	CreateCart(context.Context, Cart) (Cart, error)
	GetCart(context.Context, string) (Cart, error)
	MutateCart(context.Context, CartMutation) (Cart, error)
	FindCheckout(context.Context, string) (CheckoutRecord, bool, error)
	CommitCheckout(context.Context, CheckoutCommit) (Order, error)
	ListOrders(context.Context, OrderCursor, int) ([]Order, error)
	GetOrder(context.Context, string) (Order, error)
	CancelOrder(context.Context, CancelCommand, []messaging.Event) (Order, error)
}

type CustomerClient interface {
	Validate(context.Context, string, string, Propagation) (CustomerSnapshot, error)
}

type CatalogClient interface {
	Snapshot(context.Context, []string, Propagation) ([]CatalogSnapshot, error)
}

type InventoryClient interface {
	Reserve(context.Context, ReservationCommand, Propagation) error
	Release(context.Context, string, Propagation) error
}

type durableCheckoutStore interface {
	ClaimCheckout(context.Context, CheckoutClaim) (CheckoutRecord, bool, error)
	SaveCheckoutPrepared(context.Context, string, string, Order, ReservationCommand) error
	SaveCheckoutReserved(context.Context, string, string, ReservationResult) error
	FailCheckout(context.Context, string, string, string) error
}

type reservationResultClient interface {
	ReserveResult(context.Context, ReservationCommand, Propagation) (ReservationResult, error)
}

type ServiceOptions struct {
	Clock func() time.Time
}

type Service struct {
	store     Store
	customer  CustomerClient
	catalog   CatalogClient
	inventory InventoryClient
	clock     func() time.Time
}

func NewService(store Store, customer CustomerClient, catalog CatalogClient, inventory InventoryClient, options ServiceOptions) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{store: store, customer: customer, catalog: catalog, inventory: inventory, clock: clock}
}

func (service *Service) CreateCart(ctx context.Context, command CreateCartCommand) (Cart, error) {
	command.CustomerID = strings.TrimSpace(command.CustomerID)
	command.Currency = strings.ToUpper(strings.TrimSpace(command.Currency))
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.CustomerID == "" || len(command.Currency) != 3 ||
		command.IdempotencyKey == "" || len(command.IdempotencyKey) > 200 {
		return Cart{}, ErrInvalidCommand
	}
	now := service.clock().UTC()
	cart := Cart{
		ID:         deterministicChildID("CART", "create", command.IdempotencyKey, 0),
		CustomerID: command.CustomerID, Status: CartActive,
		Currency: command.Currency, Version: 1, ExpiresAt: now.Add(24 * time.Hour),
		Lines: []CartLine{}, IdempotencyKey: command.IdempotencyKey,
	}
	return service.store.CreateCart(ctx, cart)
}

func (service *Service) GetCart(ctx context.Context, id string) (Cart, error) {
	if strings.TrimSpace(id) == "" {
		return Cart{}, ErrCartNotFound
	}
	cart, err := service.store.GetCart(ctx, id)
	if err != nil {
		return Cart{}, err
	}
	return canonicalCart(cart), nil
}

func (service *Service) MutateCart(ctx context.Context, command CartMutation) (Cart, error) {
	command.CartID = strings.TrimSpace(command.CartID)
	command.SKU = strings.TrimSpace(command.SKU)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.CartID == "" || command.SKU == "" || command.ExpectedVersion <= 0 ||
		command.IdempotencyKey == "" ||
		len(command.IdempotencyKey) > 200 ||
		(command.Action != CartRemoveLine && command.Quantity <= 0) {
		return Cart{}, ErrInvalidCommand
	}
	switch command.Action {
	case CartAddLine, CartChangeLine, CartRemoveLine:
	default:
		return Cart{}, ErrInvalidCommand
	}
	cart, err := service.store.MutateCart(ctx, command)
	if err != nil {
		return Cart{}, err
	}
	return canonicalCart(cart), nil
}

func (service *Service) ListOrders(ctx context.Context, encodedCursor string, requestedSize int) (OrderPage, error) {
	cursor, err := decodeOrderCursor(encodedCursor)
	if err != nil {
		return OrderPage{}, err
	}
	size := normalizeOrderPageSize(requestedSize)
	orders, err := service.store.ListOrders(ctx, cursor, size+1)
	if err != nil {
		return OrderPage{}, fmt.Errorf("list orders: %w", err)
	}
	page := OrderPage{Items: orders}
	if len(orders) > size {
		page.Items = orders[:size]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeOrderCursor(OrderCursor{PlacedAt: last.PlacedAt, OrderID: last.OrderID})
	}
	for index := range page.Items {
		page.Items[index] = canonicalOrder(page.Items[index])
	}
	if page.Items == nil {
		page.Items = []Order{}
	}
	return page, nil
}

func (service *Service) GetOrder(ctx context.Context, id string) (Order, error) {
	if strings.TrimSpace(id) == "" {
		return Order{}, ErrOrderNotFound
	}
	order, err := service.store.GetOrder(ctx, id)
	if err != nil {
		return Order{}, err
	}
	return canonicalOrder(order), nil
}

func (service *Service) Checkout(ctx context.Context, command CheckoutCommand) (Order, error) {
	command = canonicalCheckoutCommand(command)
	if err := validateCheckoutCommand(command); err != nil {
		return Order{}, err
	}
	cart, err := service.store.GetCart(ctx, command.CartID)
	if err != nil {
		return Order{}, err
	}
	now := service.clock().UTC()
	requestHash := checkoutRequestHash(command, cart)
	if existing, found, err := service.store.FindCheckout(ctx, command.IdempotencyKey); err != nil {
		return Order{}, fmt.Errorf("find checkout replay: %w", err)
	} else if found && (existing.Phase == "committed" || existing.Order.OrderID != "") {
		if existing.RequestHash != requestHash {
			return Order{}, ErrIdempotencyConflict
		}
		existing.Order.Replayed = true
		return canonicalOrder(existing.Order), nil
	}
	if err := validateCheckoutCart(cart, now); err != nil {
		return Order{}, err
	}
	if durable, ok := service.store.(durableCheckoutStore); ok {
		return service.checkoutDurably(ctx, durable, command, cart, requestHash, now)
	}
	return service.checkoutLegacy(ctx, command, cart, requestHash, now)
}

func (service *Service) checkoutDurably(
	ctx context.Context,
	store durableCheckoutStore,
	command CheckoutCommand,
	cart Cart,
	requestHash string,
	now time.Time,
) (Order, error) {
	orderID := deterministicOrderID(command.IdempotencyKey)
	record, owned, err := store.ClaimCheckout(ctx, CheckoutClaim{
		IdempotencyKey: command.IdempotencyKey, RequestHash: requestHash,
		Cart: cart, OrderID: orderID, Now: now,
	})
	if err != nil {
		return Order{}, err
	}
	if record.RequestHash != requestHash {
		return Order{}, ErrIdempotencyConflict
	}
	if record.Phase == "committed" {
		record.Order.Replayed = true
		return canonicalOrder(record.Order), nil
	}
	if record.Phase == "failed" || record.Phase == "compensation_needed" {
		return Order{}, ErrUpstreamUnavailable
	}
	if !owned {
		return service.waitForCheckout(ctx, command.IdempotencyKey, requestHash)
	}
	propagation := Propagation{
		RequestID: command.RequestID, Traceparent: command.Traceparent,
		IdempotencyKey: command.IdempotencyKey, CorrelationID: command.CorrelationID,
	}
	order, reservation := record.PreparedOrder, record.PreparedReservation
	if record.Phase == "claimed" {
		customer, err := service.customer.Validate(ctx, cart.CustomerID, command.AddressID, propagation)
		if err != nil {
			code := "retryable"
			if errors.Is(err, ErrInvalidCommand) {
				code = "customer"
			}
			_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, code)
			return Order{}, fmt.Errorf("validate checkout customer: %w", err)
		}
		skus := make([]string, len(cart.Lines))
		for index := range cart.Lines {
			skus[index] = cart.Lines[index].SKU
		}
		snapshots, err := service.catalog.Snapshot(ctx, skus, propagation)
		if err != nil {
			code := "retryable"
			if errors.Is(err, ErrInvalidCommand) {
				code = "catalog"
			}
			_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, code)
			return Order{}, fmt.Errorf("snapshot checkout catalog: %w", err)
		}
		order, reservation, err = buildCheckoutOrder(
			orderID, cart, customer, snapshots, command.CouponCode, command.IdempotencyKey, now)
		if err != nil {
			_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, "money")
			return Order{}, err
		}
		if err := store.SaveCheckoutPrepared(ctx, command.IdempotencyKey, requestHash, order, reservation); err != nil {
			return Order{}, fmt.Errorf("persist checkout preparation: %w", err)
		}
		record.Phase = "prepared"
	}
	if record.Phase == "prepared" {
		propagation.IdempotencyKey = reservation.IdempotencyKey
		result, err := service.reserveInventory(ctx, reservation, propagation)
		if err != nil {
			code := "retryable"
			if errors.Is(err, ErrIdempotencyConflict) {
				code = "inventory_conflict"
			}
			_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, code)
			return Order{}, fmt.Errorf("reserve checkout inventory: %w", err)
		}
		if err := validateReservationResult(order, reservation, result); err != nil {
			_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, "invalid_reservation")
			return Order{}, err
		}
		if err := store.SaveCheckoutReserved(ctx, command.IdempotencyKey, requestHash, result); err != nil {
			return Order{}, fmt.Errorf("persist checkout reservation: %w", err)
		}
	}
	payload, err := json.Marshal(orderPlacedPayload(order))
	if err != nil {
		return Order{}, fmt.Errorf("encode order placed event: %w", err)
	}
	placed := messaging.NewEvent("order.placed.v1", order.OrderID, command.CorrelationID, "", payload,
		func() time.Time { return now })
	saved, err := service.store.CommitCheckout(ctx, CheckoutCommit{
		CartID: command.CartID, CartVersion: cart.Version, RequestHash: requestHash,
		Order: order, Event: placed,
	})
	if err == nil {
		return canonicalOrder(saved), nil
	}
	if errors.Is(err, ErrIdempotencyConflict) {
		return Order{}, err
	}
	if errors.Is(err, ErrVersionConflict) {
		_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, "compensation_needed")
		releasePropagation := propagation
		releasePropagation.CausationID = placed.ID
		if releaseErr := service.inventory.Release(ctx, order.OrderID, releasePropagation); releaseErr != nil {
			return Order{}, fmt.Errorf("%w: release inventory after checkout conflict: %v",
				ErrUpstreamUnavailable, releaseErr)
		}
		_ = store.FailCheckout(ctx, command.IdempotencyKey, requestHash, "cart_conflict")
		return Order{}, err
	}
	// The commit result may be unknown. A read-after-error makes a completed
	// commit observable; otherwise the deterministic order and reservation
	// identities let the caller safely retry without duplicating stock.
	existing, found, lookupErr := service.store.FindCheckout(ctx, command.IdempotencyKey)
	if lookupErr == nil && found {
		if existing.RequestHash != requestHash {
			return Order{}, ErrIdempotencyConflict
		}
		existing.Order.Replayed = true
		return canonicalOrder(existing.Order), nil
	}
	return Order{}, fmt.Errorf("%w: %v", ErrCheckoutUncertain, err)
}

func (service *Service) waitForCheckout(
	ctx context.Context,
	key string,
	hash string,
) (Order, error) {
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Order{}, fmt.Errorf("%w: %v", ErrCheckoutUncertain, ctx.Err())
		case <-timeout.C:
			return Order{}, ErrCheckoutUncertain
		case <-ticker.C:
			record, found, err := service.store.FindCheckout(ctx, key)
			if err != nil {
				return Order{}, err
			}
			if !found || record.RequestHash != hash {
				return Order{}, ErrIdempotencyConflict
			}
			switch record.Phase {
			case "committed":
				record.Order.Replayed = true
				return canonicalOrder(record.Order), nil
			case "failed", "compensation_needed":
				return Order{}, ErrUpstreamUnavailable
			}
		}
	}
}

func (service *Service) checkoutLegacy(
	ctx context.Context,
	command CheckoutCommand,
	cart Cart,
	requestHash string,
	now time.Time,
) (Order, error) {
	if existing, found, err := service.store.FindCheckout(ctx, command.IdempotencyKey); err != nil {
		return Order{}, fmt.Errorf("find checkout replay: %w", err)
	} else if found {
		if existing.RequestHash != requestHash {
			return Order{}, ErrIdempotencyConflict
		}
		existing.Order.Replayed = true
		return canonicalOrder(existing.Order), nil
	}
	propagation := Propagation{
		RequestID: command.RequestID, Traceparent: command.Traceparent,
		IdempotencyKey: command.IdempotencyKey, CorrelationID: command.CorrelationID,
	}
	customer, err := service.customer.Validate(ctx, cart.CustomerID, command.AddressID, propagation)
	if err != nil {
		return Order{}, fmt.Errorf("validate checkout customer: %w", err)
	}
	skus := make([]string, len(cart.Lines))
	for index := range cart.Lines {
		skus[index] = cart.Lines[index].SKU
	}
	snapshots, err := service.catalog.Snapshot(ctx, skus, propagation)
	if err != nil {
		return Order{}, fmt.Errorf("snapshot checkout catalog: %w", err)
	}
	order, reservation, err := buildCheckoutOrder(
		deterministicOrderID(command.IdempotencyKey), cart, customer, snapshots,
		command.CouponCode, command.IdempotencyKey, now)
	if err != nil {
		return Order{}, err
	}
	propagation.IdempotencyKey = reservation.IdempotencyKey
	if _, err := service.reserveInventory(ctx, reservation, propagation); err != nil {
		return Order{}, fmt.Errorf("reserve checkout inventory: %w", err)
	}
	payload, _ := json.Marshal(orderPlacedPayload(order))
	placed := messaging.NewEvent("order.placed.v1", order.OrderID, command.CorrelationID, "", payload,
		func() time.Time { return now })
	saved, err := service.store.CommitCheckout(ctx, CheckoutCommit{
		CartID: command.CartID, CartVersion: cart.Version, RequestHash: requestHash,
		Order: order, Event: placed,
	})
	if err == nil || errors.Is(err, ErrIdempotencyConflict) {
		return saved, err
	}
	if errors.Is(err, ErrVersionConflict) {
		if releaseErr := service.inventory.Release(ctx, order.OrderID, propagation); releaseErr != nil {
			return Order{}, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, releaseErr)
		}
		return Order{}, err
	}
	if existing, found, lookupErr := service.store.FindCheckout(ctx, command.IdempotencyKey); lookupErr == nil && found && existing.RequestHash == requestHash && existing.Order.OrderID != "" {
		existing.Order.Replayed = true
		return existing.Order, nil
	}
	return Order{}, fmt.Errorf("%w: %v", ErrCheckoutUncertain, err)
}

func (service *Service) reserveInventory(
	ctx context.Context,
	command ReservationCommand,
	propagation Propagation,
) (ReservationResult, error) {
	if client, ok := service.inventory.(reservationResultClient); ok {
		return client.ReserveResult(ctx, command, propagation)
	}
	if err := service.inventory.Reserve(ctx, command, propagation); err != nil {
		return ReservationResult{}, err
	}
	allocations := make([]ReservationAllocation, len(command.Lines))
	for index, line := range command.Lines {
		allocations[index] = ReservationAllocation{
			AllocationID: deterministicChildID("RES", command.OrderID, line.SKU, index),
			SKU:          line.SKU, Quantity: line.Quantity, Status: "active",
		}
	}
	return ReservationResult{OrderID: command.OrderID, Allocations: allocations}, nil
}

func validateReservationResult(
	order Order,
	command ReservationCommand,
	result ReservationResult,
) error {
	if result.OrderID != order.OrderID || len(result.Allocations) != len(command.Lines) {
		return fmt.Errorf("%w: inventory reservation identity mismatch", ErrInvalidCommand)
	}
	expected := make(map[string]int64, len(command.Lines))
	for _, line := range command.Lines {
		expected[line.SKU] = line.Quantity
	}
	seen := make(map[string]bool, len(result.Allocations))
	for _, allocation := range result.Allocations {
		if allocation.AllocationID == "" || allocation.Status != "active" ||
			seen[allocation.SKU] || expected[allocation.SKU] != allocation.Quantity {
			return fmt.Errorf("%w: inventory did not return active allocations", ErrInvalidCommand)
		}
		seen[allocation.SKU] = true
	}
	return nil
}

func (service *Service) Cancel(ctx context.Context, command CancelCommand) (Order, error) {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.Reason = strings.TrimSpace(command.Reason)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	if command.OrderID == "" || command.Reason == "" || command.IdempotencyKey == "" ||
		len(command.IdempotencyKey) > 200 {
		return Order{}, ErrInvalidCommand
	}
	return service.store.CancelOrder(ctx, command, nil)
}

func buildCheckoutOrder(
	orderID string,
	cart Cart,
	customer CustomerSnapshot,
	snapshots []CatalogSnapshot,
	couponCode string,
	key string,
	now time.Time,
) (Order, ReservationCommand, error) {
	if customer.ID != cart.CustomerID || strings.TrimSpace(customer.Email) == "" ||
		strings.TrimSpace(customer.Name) == "" || len(customer.Address) == 0 ||
		!json.Valid(customer.Address) || customer.Address[0] != '{' {
		return Order{}, ReservationCommand{}, fmt.Errorf("%w: invalid customer snapshot", ErrInvalidCommand)
	}
	bySKU := make(map[string]CatalogSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		if snapshot.SKU == "" || snapshot.Title == "" || snapshot.UnitPriceMinor < 0 ||
			snapshot.Currency != cart.Currency {
			return Order{}, ReservationCommand{}, fmt.Errorf("%w: invalid catalog snapshot", ErrInvalidCommand)
		}
		if _, duplicate := bySKU[snapshot.SKU]; duplicate {
			return Order{}, ReservationCommand{}, fmt.Errorf("%w: duplicate catalog SKU %s", ErrInvalidCommand, snapshot.SKU)
		}
		bySKU[snapshot.SKU] = snapshot
	}
	lines := make([]OrderLine, 0, len(cart.Lines))
	moneyLines := make([]Line, 0, len(cart.Lines))
	reservationLines := make([]ReservationLine, 0, len(cart.Lines))
	grossAmounts := make([]int64, 0, len(cart.Lines))
	for index, cartLine := range cart.Lines {
		snapshot, found := bySKU[cartLine.SKU]
		if !found {
			return Order{}, ReservationCommand{}, fmt.Errorf("%w: catalog snapshot missing SKU %s", ErrInvalidCommand, cartLine.SKU)
		}
		gross, ok := checkedMultiply(cartLine.Quantity, snapshot.UnitPriceMinor)
		if !ok {
			return Order{}, ReservationCommand{}, ErrInvalidMoney
		}
		line := OrderLine{
			ID:  deterministicChildID("ITEM", orderID, cartLine.SKU, index),
			SKU: cartLine.SKU, Title: snapshot.Title, Quantity: cartLine.Quantity,
			UnitPriceMinor: snapshot.UnitPriceMinor, TotalMinor: gross,
			ProductID: snapshot.ProductID, ProductTitle: snapshot.ProductTitle,
			VariantTitle: snapshot.VariantTitle, Attributes: snapshot.Attributes,
			WeightGrams: snapshot.WeightGrams, Channel: snapshot.Channel,
		}
		lines = append(lines, line)
		moneyLines = append(moneyLines, Line{Quantity: line.Quantity, UnitPriceMinor: line.UnitPriceMinor})
		reservationLines = append(reservationLines, ReservationLine{SKU: line.SKU, Quantity: line.Quantity})
		grossAmounts = append(grossAmounts, gross)
	}
	if couponCode != "" {
		if couponCode != "WELCOME10" {
			return Order{}, ReservationCommand{}, fmt.Errorf("%w: unsupported coupon", ErrInvalidCommand)
		}
		allocations, err := AllocatePercentageDiscount(grossAmounts, 1000)
		if err != nil {
			return Order{}, ReservationCommand{}, err
		}
		for index := range lines {
			lines[index].DiscountMinor = allocations[index]
			lines[index].TotalMinor -= allocations[index]
			moneyLines[index].DiscountMinor = allocations[index]
		}
	}
	shipping := int64(800)
	var preliminary int64
	for _, line := range moneyLines {
		gross, ok := checkedMultiply(line.Quantity, line.UnitPriceMinor)
		if !ok {
			return Order{}, ReservationCommand{}, ErrInvalidMoney
		}
		preliminary, ok = checkedAdd(preliminary, gross)
		if !ok {
			return Order{}, ReservationCommand{}, ErrInvalidMoney
		}
	}
	if preliminary >= 10000 {
		shipping = 0
	}
	var addressRegion struct {
		CountryCode string `json:"country_code"`
		Province    string `json:"province"`
		Region      string `json:"region"`
	}
	if err := json.Unmarshal(customer.Address, &addressRegion); err != nil {
		return Order{}, ReservationCommand{}, ErrInvalidCommand
	}
	region := addressRegion.Province
	if region == "" {
		region = addressRegion.Region
	}
	taxable := preliminary
	for _, line := range lines {
		taxable -= line.DiscountMinor
	}
	rate, tax, err := RegionalTax(addressRegion.CountryCode, region, taxable)
	if err != nil {
		return Order{}, ReservationCommand{}, err
	}
	netAmounts := make([]int64, len(lines))
	for index := range lines {
		netAmounts[index] = lines[index].TotalMinor
	}
	taxAllocations, err := AllocatePercentageDiscount(netAmounts, rate)
	if err != nil {
		return Order{}, ReservationCommand{}, err
	}
	for index := range lines {
		lines[index].TaxRateBPS, lines[index].TaxMinor = rate, taxAllocations[index]
	}
	totals, err := CalculateTotals(moneyLines, shipping, tax)
	if err != nil {
		return Order{}, ReservationCommand{}, err
	}
	if err := reconcileOrderAmounts(lines, totals); err != nil {
		return Order{}, ReservationCommand{}, err
	}
	order := canonicalOrder(Order{
		OrderID: orderID, Number: deterministicOrderNumber(orderID), CustomerID: cart.CustomerID,
		Status: "pending", PaymentStatus: "pending", FulfillmentStatus: "unfulfilled",
		Currency: cart.Currency, SubtotalMinor: totals.Subtotal, DiscountMinor: totals.Discount,
		ShippingMinor: totals.Shipping, TaxMinor: totals.Tax, TotalMinor: totals.Total,
		Customer: customer, ShippingAddress: append(json.RawMessage(nil), customer.Address...),
		Lines: lines, IdempotencyKey: key, PlacedAt: now, SagaState: "paying",
		CouponCode: couponCode,
	})
	return order, ReservationCommand{
		OrderID: order.OrderID, IdempotencyKey: "inventory:" + key, Lines: reservationLines,
	}, nil
}

func reconcileOrderAmounts(lines []OrderLine, totals Totals) error {
	var gross, discount, net int64
	for _, line := range lines {
		lineGross, ok := checkedMultiply(line.Quantity, line.UnitPriceMinor)
		if !ok || line.TotalMinor != lineGross-line.DiscountMinor {
			return fmt.Errorf("%w: line total does not reconcile", ErrInvalidMoney)
		}
		gross, ok = checkedAdd(gross, lineGross)
		if !ok {
			return ErrInvalidMoney
		}
		discount, ok = checkedAdd(discount, line.DiscountMinor)
		if !ok {
			return ErrInvalidMoney
		}
		net, ok = checkedAdd(net, line.TotalMinor)
		if !ok {
			return ErrInvalidMoney
		}
	}
	if gross != totals.Subtotal || discount != totals.Discount || net != totals.Subtotal-totals.Discount ||
		totals.Total != totals.Subtotal-totals.Discount+totals.Shipping+totals.Tax {
		return fmt.Errorf("%w: order total does not reconcile", ErrInvalidMoney)
	}
	return nil
}

func validateCheckoutCommand(command CheckoutCommand) error {
	if command.CartID == "" || command.AddressID == "" || command.IdempotencyKey == "" ||
		len(command.IdempotencyKey) > 200 {
		return ErrInvalidCommand
	}
	return nil
}

func validateCheckoutCart(cart Cart, now time.Time) error {
	if cart.Status != CartActive || len(cart.Lines) == 0 || cart.Currency == "" ||
		cart.Version <= 0 || (!cart.ExpiresAt.IsZero() && !cart.ExpiresAt.After(now)) {
		return ErrInvalidCommand
	}
	seen := make(map[string]bool, len(cart.Lines))
	for _, line := range cart.Lines {
		if strings.TrimSpace(line.SKU) == "" || line.Quantity <= 0 || seen[line.SKU] {
			return ErrInvalidCommand
		}
		seen[line.SKU] = true
	}
	return nil
}

func canonicalCheckoutCommand(command CheckoutCommand) CheckoutCommand {
	command.CartID = strings.TrimSpace(command.CartID)
	command.AddressID = strings.TrimSpace(command.AddressID)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.RequestID = strings.TrimSpace(command.RequestID)
	command.Traceparent = strings.TrimSpace(command.Traceparent)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	command.CouponCode = strings.ToUpper(strings.TrimSpace(command.CouponCode))
	if command.CorrelationID == "" {
		command.CorrelationID = command.RequestID
	}
	return command
}

func checkoutRequestHash(command CheckoutCommand, cart Cart) string {
	cart = canonicalCart(cart)
	body, _ := json.Marshal(struct {
		CartID      string     `json:"cart_id"`
		CartVersion int64      `json:"cart_version"`
		CustomerID  string     `json:"customer_id"`
		Currency    string     `json:"currency"`
		Lines       []CartLine `json:"lines"`
		AddressID   string     `json:"address_id"`
		CouponCode  string     `json:"coupon_code"`
	}{
		CartID: command.CartID, CartVersion: cart.Version, CustomerID: cart.CustomerID,
		Currency: cart.Currency, Lines: cart.Lines, AddressID: command.AddressID,
		CouponCode: command.CouponCode,
	})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func deterministicOrderID(key string) string {
	return deterministicChildID("ORD", "checkout", key, 0)
}

func deterministicOrderNumber(orderID string) string {
	sum := sha256.Sum256([]byte(orderID))
	return "O" + strings.ToUpper(hex.EncodeToString(sum[:8]))
}

func deterministicChildID(prefix string, parent string, value string, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", parent, value, index)))
	return prefix + "-" + strings.ToUpper(hex.EncodeToString(sum[:8]))
}

func orderPlacedPayload(order Order) map[string]any {
	lines := make([]map[string]any, len(order.Lines))
	for index, line := range order.Lines {
		lines[index] = map[string]any{
			"order_item_id": line.ID, "sku": line.SKU, "quantity": line.Quantity,
			"unit_price_minor": line.UnitPriceMinor, "total_minor": line.TotalMinor,
		}
	}
	return map[string]any{
		"order_id": order.OrderID, "customer_id": order.CustomerID, "currency": order.Currency,
		"total_minor": order.TotalMinor, "lines": lines,
	}
}

func newOrderEvent(kind string, order Order, correlationID, causationID string, payload any, now time.Time) messaging.Event {
	body, _ := json.Marshal(payload)
	return messaging.NewEvent(kind, order.OrderID, correlationID, causationID, body, func() time.Time { return now })
}

func canonicalOrder(order Order) Order {
	order.PlacedAt = order.PlacedAt.UTC()
	order.ShippingAddress = append(json.RawMessage(nil), order.ShippingAddress...)
	order.Customer.Address = append(json.RawMessage(nil), order.Customer.Address...)
	order.Lines = append([]OrderLine(nil), order.Lines...)
	sort.Slice(order.Lines, func(left, right int) bool { return order.Lines[left].ID < order.Lines[right].ID })
	if order.Lines == nil {
		order.Lines = []OrderLine{}
	}
	return order
}

func canonicalCart(cart Cart) Cart {
	cart.ExpiresAt = cart.ExpiresAt.UTC()
	cart.Lines = append([]CartLine(nil), cart.Lines...)
	sort.Slice(cart.Lines, func(left, right int) bool { return cart.Lines[left].SKU < cart.Lines[right].SKU })
	if cart.Lines == nil {
		cart.Lines = []CartLine{}
	}
	return cart
}

func encodeOrderCursor(cursor OrderCursor) string {
	body, _ := json.Marshal(struct {
		Version  int       `json:"v"`
		PlacedAt time.Time `json:"placed_at"`
		OrderID  string    `json:"order_id"`
	}{Version: 1, PlacedAt: cursor.PlacedAt.UTC(), OrderID: cursor.OrderID})
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeOrderCursor(encoded string) (OrderCursor, error) {
	if encoded == "" {
		return OrderCursor{}, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return OrderCursor{}, errInvalidOrderCursor
	}
	var envelope struct {
		Version  int       `json:"v"`
		PlacedAt time.Time `json:"placed_at"`
		OrderID  string    `json:"order_id"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Version != 1 ||
		envelope.PlacedAt.IsZero() || envelope.OrderID == "" ||
		strings.TrimSpace(envelope.OrderID) != envelope.OrderID {
		return OrderCursor{}, errInvalidOrderCursor
	}
	return OrderCursor{PlacedAt: envelope.PlacedAt.UTC(), OrderID: envelope.OrderID}, nil
}

func normalizeOrderPageSize(requested int) int {
	if requested <= 0 {
		return DefaultPageSize
	}
	if requested > MaxPageSize {
		return MaxPageSize
	}
	return requested
}

func randomHex(bytesCount int) string {
	body := make([]byte, bytesCount)
	if _, err := rand.Read(body); err == nil {
		return strings.ToUpper(hex.EncodeToString(body))
	}
	sum := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	return strings.ToUpper(hex.EncodeToString(sum[:bytesCount]))
}
