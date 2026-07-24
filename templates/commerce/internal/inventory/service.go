package inventory

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	DefaultPageSize       = 20
	MaxPageSize           = 100
	DefaultReservationTTL = 15 * time.Minute
	maxReservationLines   = 100
)

var (
	ErrSKUNotFound         = errors.New("inventory SKU not found")
	ErrInsufficientStock   = errors.New("insufficient inventory stock")
	ErrIdempotencyConflict = errors.New("inventory idempotency conflict")
	ErrOrderTerminal       = errors.New("inventory order is terminal")
	ErrReservationNotFound = errors.New("inventory reservation not found")
	ErrInvalidCommand      = errors.New("invalid inventory command")
	errInvalidCursor       = errors.New("invalid inventory cursor")
)

type InventoryLevel struct {
	SKU          string    `json:"sku"`
	LocationID   string    `json:"location_id"`
	LocationName string    `json:"location_name"`
	Priority     int       `json:"priority"`
	OnHand       int64     `json:"on_hand"`
	Reserved     int64     `json:"reserved"`
	Available    int64     `json:"available"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

type InventoryCursor struct {
	SKU        string `json:"sku"`
	LocationID string `json:"location_id"`
}

type InventoryPage struct {
	Items      []InventoryLevel `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type ReserveLine struct {
	SKU      string `json:"sku"`
	Quantity int64  `json:"quantity"`
}

type ReserveCommand struct {
	OrderID        string
	IdempotencyKey string
	CorrelationID  string
	Lines          []ReserveLine
	OccurredAt     time.Time
	ExpiresAt      time.Time
}

type ReservationAllocation struct {
	ID         string           `json:"reservation_id"`
	OrderID    string           `json:"order_id"`
	SKU        string           `json:"sku"`
	LocationID string           `json:"location_id"`
	Quantity   int64            `json:"quantity"`
	State      ReservationState `json:"status"`
	ExpiresAt  time.Time        `json:"expires_at"`
}

type ReservationResult struct {
	OrderID     string                  `json:"order_id"`
	Allocations []ReservationAllocation `json:"allocations"`
	Existing    bool                    `json:"-"`
}

type Store interface {
	ListLevels(context.Context, InventoryCursor, int) ([]InventoryLevel, error)
	GetLevelsBySKU(context.Context, string) ([]InventoryLevel, error)
	ListReservationsByOrder(context.Context, string) ([]ReservationAllocation, error)
	Reserve(context.Context, ReserveCommand) (ReservationResult, error)
	TransitionOrder(context.Context, string, ReservationEvent, time.Time) ([]ReservationAllocation, bool, error)
}

type Service struct {
	store Store
	clock func() time.Time
}

func NewService(store Store, clock func() time.Time) *Service {
	if clock == nil {
		clock = time.Now
	}
	return &Service{store: store, clock: clock}
}

func (service *Service) ListLevels(ctx context.Context, encodedCursor string, requestedSize int) (InventoryPage, error) {
	cursor, err := decodeInventoryCursor(encodedCursor)
	if err != nil {
		return InventoryPage{}, err
	}
	size := normalizeInventoryPageSize(requestedSize)
	levels, err := service.store.ListLevels(ctx, cursor, size+1)
	if err != nil {
		return InventoryPage{}, fmt.Errorf("list inventory levels: %w", err)
	}
	page := InventoryPage{Items: levels}
	if len(levels) > size {
		page.Items = levels[:size]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = encodeInventoryCursor(InventoryCursor{SKU: last.SKU, LocationID: last.LocationID})
	}
	if page.Items == nil {
		page.Items = []InventoryLevel{}
	}
	return page, nil
}

func (service *Service) GetLevelsBySKU(ctx context.Context, sku string) ([]InventoryLevel, error) {
	if strings.TrimSpace(sku) == "" {
		return nil, ErrSKUNotFound
	}
	return service.store.GetLevelsBySKU(ctx, sku)
}

func (service *Service) ListReservationsByOrder(ctx context.Context, orderID string) ([]ReservationAllocation, error) {
	if strings.TrimSpace(orderID) == "" {
		return nil, ErrReservationNotFound
	}
	return service.store.ListReservationsByOrder(ctx, orderID)
}

func (service *Service) Reserve(ctx context.Context, command ReserveCommand) (ReservationResult, error) {
	command.OrderID = strings.TrimSpace(command.OrderID)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.OrderID == "" || command.IdempotencyKey == "" ||
		len(command.Lines) == 0 || len(command.Lines) > maxReservationLines {
		return ReservationResult{}, ErrInvalidCommand
	}
	quantities := make(map[string]int64, len(command.Lines))
	for _, line := range command.Lines {
		sku := strings.TrimSpace(line.SKU)
		if sku == "" || line.Quantity <= 0 || quantities[sku] > int64(^uint64(0)>>1)-line.Quantity {
			return ReservationResult{}, ErrInvalidCommand
		}
		quantities[sku] += line.Quantity
	}
	command.Lines = make([]ReserveLine, 0, len(quantities))
	for sku, quantity := range quantities {
		command.Lines = append(command.Lines, ReserveLine{SKU: sku, Quantity: quantity})
	}
	sort.Slice(command.Lines, func(i, j int) bool { return command.Lines[i].SKU < command.Lines[j].SKU })
	command.OccurredAt = service.clock().UTC()
	if command.ExpiresAt.IsZero() {
		command.ExpiresAt = command.OccurredAt.Add(DefaultReservationTTL)
	}
	if !command.ExpiresAt.After(command.OccurredAt) {
		return ReservationResult{}, ErrInvalidCommand
	}
	if command.CorrelationID == "" {
		command.CorrelationID = command.OrderID
	}
	return service.store.Reserve(ctx, command)
}

func (service *Service) TransitionOrder(ctx context.Context, orderID string, event ReservationEvent) (ReservationResult, error) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return ReservationResult{}, ErrInvalidCommand
	}
	switch event {
	case ReservationRelease, ReservationCommit, ReservationExpire:
	default:
		return ReservationResult{}, ErrInvalidCommand
	}
	allocations, _, err := service.store.TransitionOrder(ctx, orderID, event, service.clock().UTC())
	if err != nil {
		return ReservationResult{}, err
	}
	return ReservationResult{OrderID: orderID, Allocations: allocations}, nil
}

type inventoryCursorEnvelope struct {
	Version int             `json:"v"`
	After   InventoryCursor `json:"after"`
}

func encodeInventoryCursor(cursor InventoryCursor) string {
	body, _ := json.Marshal(inventoryCursorEnvelope{Version: 1, After: cursor})
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeInventoryCursor(cursor string) (InventoryCursor, error) {
	if cursor == "" {
		return InventoryCursor{}, nil
	}
	body, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return InventoryCursor{}, errInvalidCursor
	}
	var envelope inventoryCursorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Version != 1 ||
		envelope.After.SKU == "" || envelope.After.LocationID == "" ||
		strings.TrimSpace(envelope.After.SKU) != envelope.After.SKU ||
		strings.TrimSpace(envelope.After.LocationID) != envelope.After.LocationID {
		return InventoryCursor{}, errInvalidCursor
	}
	return envelope.After, nil
}

func normalizeInventoryPageSize(requested int) int {
	if requested <= 0 {
		return DefaultPageSize
	}
	if requested > MaxPageSize {
		return MaxPageSize
	}
	return requested
}
