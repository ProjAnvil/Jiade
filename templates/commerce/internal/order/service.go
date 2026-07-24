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
	ID         string     `json:"cart_id"`
	CustomerID string     `json:"customer_id"`
	Status     CartStatus `json:"status"`
	Currency   string     `json:"currency"`
	Version    int64      `json:"version"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Lines      []CartLine `json:"lines"`
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
}

type CustomerSnapshot struct {
	ID      string          `json:"customer_id"`
	Email   string          `json:"email"`
	Name    string          `json:"name"`
	Phone   string          `json:"phone,omitempty"`
	Address json.RawMessage `json:"address"`
}

type CatalogSnapshot struct {
	SKU            string `json:"sku"`
	Title          string `json:"title"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	Currency       string `json:"currency"`
}

type OrderLine struct {
	ID             string `json:"order_item_id"`
	SKU            string `json:"sku"`
	Title          string `json:"title"`
	Quantity       int64  `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	DiscountMinor  int64  `json:"discount_minor"`
	TotalMinor     int64  `json:"total_minor"`
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
}

type CreateCartCommand struct {
	CustomerID string
	Currency   string
}

type CancelCommand struct {
	OrderID       string
	Reason        string
	CorrelationID string
	CausationID   string
}

type CheckoutRecord struct {
	RequestHash string
	Order       Order
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
	if command.CustomerID == "" || len(command.Currency) != 3 {
		return Cart{}, ErrInvalidCommand
	}
	now := service.clock().UTC()
	cart := Cart{
		ID: "CART-" + randomHex(8), CustomerID: command.CustomerID, Status: CartActive,
		Currency: command.Currency, Version: 1, ExpiresAt: now.Add(24 * time.Hour),
		Lines: []CartLine{},
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
	if command.CartID == "" || command.SKU == "" || command.ExpectedVersion <= 0 ||
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
	requestHash := checkoutRequestHash(command)
	if existing, found, err := service.store.FindCheckout(ctx, command.IdempotencyKey); err != nil {
		return Order{}, fmt.Errorf("find checkout replay: %w", err)
	} else if found {
		if existing.RequestHash != requestHash {
			return Order{}, ErrIdempotencyConflict
		}
		existing.Order.Replayed = true
		return canonicalOrder(existing.Order), nil
	}

	cart, err := service.store.GetCart(ctx, command.CartID)
	if err != nil {
		return Order{}, err
	}
	now := service.clock().UTC()
	if err := validateCheckoutCart(cart, now); err != nil {
		return Order{}, err
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
	orderID := deterministicOrderID(command.IdempotencyKey)
	order, reservation, err := buildCheckoutOrder(orderID, cart, customer, snapshots, command.IdempotencyKey, now)
	if err != nil {
		return Order{}, err
	}
	propagation.IdempotencyKey = reservation.IdempotencyKey
	if err := service.inventory.Reserve(ctx, reservation, propagation); err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return Order{}, ErrIdempotencyConflict
		}
		return Order{}, fmt.Errorf("reserve checkout inventory: %w", err)
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
		releasePropagation := propagation
		releasePropagation.CausationID = placed.ID
		if releaseErr := service.inventory.Release(ctx, order.OrderID, releasePropagation); releaseErr != nil {
			return Order{}, errors.Join(err, fmt.Errorf("release inventory after checkout conflict: %w", releaseErr))
		}
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

func (service *Service) Cancel(ctx context.Context, command CancelCommand) (Order, error) {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.Reason = strings.TrimSpace(command.Reason)
	command.CorrelationID = strings.TrimSpace(command.CorrelationID)
	if command.OrderID == "" || command.Reason == "" {
		return Order{}, ErrInvalidCommand
	}
	order, err := service.store.GetOrder(ctx, command.OrderID)
	if err != nil {
		return Order{}, err
	}
	if order.Status == "cancelled" {
		return order, nil
	}
	if order.Status != "pending" && order.Status != "confirmed" {
		return Order{}, ErrInvalidCommand
	}
	now := service.clock().UTC()
	events := make([]messaging.Event, 0, 4)
	cancelled := newOrderEvent("order.cancelled.v1", order, command.CorrelationID, command.CausationID,
		map[string]any{"order_id": order.OrderID, "reason": command.Reason}, now)
	events = append(events, cancelled)
	events = append(events, newOrderEvent("inventory.release-requested.v1", order, command.CorrelationID, cancelled.ID,
		map[string]any{"order_id": order.OrderID, "reason": command.Reason}, now))
	if order.PaymentStatus == "paid" || order.PaymentStatus == "authorized" ||
		order.PaymentStatus == "partially_refunded" {
		events = append(events,
			newOrderEvent("payment.refund-requested.v1", order, command.CorrelationID, cancelled.ID,
				map[string]any{"order_id": order.OrderID, "amount_minor": order.TotalMinor, "reason": command.Reason}, now),
			newOrderEvent("fulfillment.cancel-requested.v1", order, command.CorrelationID, cancelled.ID,
				map[string]any{"order_id": order.OrderID, "reason": command.Reason}, now),
		)
	}
	return service.store.CancelOrder(ctx, command, events)
}

func buildCheckoutOrder(
	orderID string,
	cart Cart,
	customer CustomerSnapshot,
	snapshots []CatalogSnapshot,
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
		}
		lines = append(lines, line)
		moneyLines = append(moneyLines, Line{Quantity: line.Quantity, UnitPriceMinor: line.UnitPriceMinor})
		reservationLines = append(reservationLines, ReservationLine{SKU: line.SKU, Quantity: line.Quantity})
	}
	shipping := int64(800)
	var preliminary int64
	for _, line := range moneyLines {
		gross, _ := checkedMultiply(line.Quantity, line.UnitPriceMinor)
		preliminary, _ = checkedAdd(preliminary, gross)
	}
	if preliminary >= 10000 {
		shipping = 0
	}
	totals, err := CalculateTotals(moneyLines, shipping, 0)
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
	if command.CorrelationID == "" {
		command.CorrelationID = command.RequestID
	}
	return command
}

func checkoutRequestHash(command CheckoutCommand) string {
	body, _ := json.Marshal(struct {
		CartID    string `json:"cart_id"`
		AddressID string `json:"address_id"`
	}{CartID: command.CartID, AddressID: command.AddressID})
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
