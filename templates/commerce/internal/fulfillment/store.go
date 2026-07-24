package fulfillment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore persists fulfillment state in the dedicated fulfillment
// database. All mutations run in a single transaction and the derived Outbox
// events are inserted alongside the domain rows so the relay only observes
// them post-commit.
type PostgresStore struct {
	pool  *pgxpool.Pool
	clock func() time.Time
}

// NewPostgresStore constructs a store bound to pool. clock defaults to time.Now.
func NewPostgresStore(pool *pgxpool.Pool, clock func() time.Time) *PostgresStore {
	if clock == nil {
		clock = time.Now
	}
	return &PostgresStore{pool: pool, clock: clock}
}

func (store *PostgresStore) assert() error {
	if store == nil || store.pool == nil {
		return errors.New("fulfillment postgres store is unavailable")
	}
	return nil
}

func (store *PostgresStore) FindFulfillment(ctx context.Context, idempotencyKey string) (FulfillOutcome, bool, error) {
	if err := store.assert(); err != nil {
		return FulfillOutcome{}, false, err
	}
	// The fulfillment side does not persist the inbound idempotency_key on a
	// domain row (the schema has no such column). The transactional Inbox on
	// consumer-side guarantees at-least-once dedupe; the conditional inserts
	// below handle a duplicate that races between two consumer workers. Treat
	// FindFulfillment as "no prior write" so the insert path is the single
	// source of truth.
	return FulfillOutcome{}, false, nil
}

func (store *PostgresStore) GetFulfillmentsByOrder(ctx context.Context, orderID string) ([]FulfillmentOrder, error) {
	if err := store.assert(); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT fulfillment_id, order_id, location_id, status, created_at
		FROM fulfillment_order
		WHERE order_id = $1
		ORDER BY location_id, fulfillment_id`, orderID)
	if err != nil {
		return nil, fmt.Errorf("list fulfillment orders: %w", err)
	}
	defer rows.Close()
	orders := make([]FulfillmentOrder, 0)
	for rows.Next() {
		var order FulfillmentOrder
		if err := rows.Scan(&order.FulfillmentID, &order.OrderID, &order.LocationID,
			&order.Status, &order.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan fulfillment order: %w", err)
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fulfillment orders: %w", err)
	}
	return orders, nil
}

// GetFulfillmentByID loads a single fulfillment_order by its primary key.
func (store *PostgresStore) GetFulfillmentByID(ctx context.Context, fulfillmentID string) (FulfillmentOrder, bool, error) {
	if err := store.assert(); err != nil {
		return FulfillmentOrder{}, false, err
	}
	var order FulfillmentOrder
	err := store.pool.QueryRow(ctx, `
		SELECT fulfillment_id, order_id, location_id, status, created_at
		FROM fulfillment_order
		WHERE fulfillment_id = $1`, fulfillmentID).
		Scan(&order.FulfillmentID, &order.OrderID, &order.LocationID,
			&order.Status, &order.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return FulfillmentOrder{}, false, nil
	}
	if err != nil {
		return FulfillmentOrder{}, false, fmt.Errorf("load fulfillment order: %w", err)
	}
	return order, true, nil
}

// ListItemsByFulfillment loads the items recorded for fulfillmentID.
func (store *PostgresStore) ListItemsByFulfillment(ctx context.Context, fulfillmentID string) ([]FulfillmentItem, error) {
	if err := store.assert(); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT fulfillment_id, order_item_id, sku, quantity
		FROM fulfillment_item
		WHERE fulfillment_id = $1
		ORDER BY order_item_id`, fulfillmentID)
	if err != nil {
		return nil, fmt.Errorf("list fulfillment items: %w", err)
	}
	defer rows.Close()
	items := make([]FulfillmentItem, 0)
	for rows.Next() {
		var item FulfillmentItem
		if err := rows.Scan(&item.FulfillmentID, &item.OrderItemID, &item.SKU, &item.Quantity); err != nil {
			return nil, fmt.Errorf("scan fulfillment item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fulfillment items: %w", err)
	}
	return items, nil
}

// ListPackagesByFulfillment loads the packages recorded for fulfillmentID.
func (store *PostgresStore) ListPackagesByFulfillment(ctx context.Context, fulfillmentID string) ([]PackageDimension, error) {
	if err := store.assert(); err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT package_id, fulfillment_id, weight_grams, length_mm, width_mm, height_mm
		FROM package
		WHERE fulfillment_id = $1
		ORDER BY package_id`, fulfillmentID)
	if err != nil {
		return nil, fmt.Errorf("list packages: %w", err)
	}
	defer rows.Close()
	packages := make([]PackageDimension, 0)
	for rows.Next() {
		var pkg PackageDimension
		if err := rows.Scan(&pkg.PackageID, &pkg.FulfillmentID,
			&pkg.WeightGrams, &pkg.LengthMM, &pkg.WidthMM, &pkg.HeightMM); err != nil {
			return nil, fmt.Errorf("scan package: %w", err)
		}
		packages = append(packages, pkg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate packages: %w", err)
	}
	return packages, nil
}

// GetShipmentByFulfillment loads the shipment projection recorded for
// fulfillmentID, including its packages and tracking events.
func (store *PostgresStore) GetShipmentByFulfillment(ctx context.Context, fulfillmentID string) (*Shipment, error) {
	if err := store.assert(); err != nil {
		return nil, err
	}
	row := store.pool.QueryRow(ctx, `
		SELECT shipment_id, fulfillment_id, carrier, tracking_number, status,
		       shipped_at, delivered_at
		FROM shipment
		WHERE fulfillment_id = $1
		ORDER BY shipped_at DESC NULLS LAST, shipment_id
		LIMIT 1`, fulfillmentID)
	var shipment Shipment
	var status string
	var shippedAt, deliveredAt *time.Time
	if err := row.Scan(&shipment.ShipmentID, &shipment.FulfillmentID, &shipment.Carrier,
		&shipment.TrackingNumber, &status, &shippedAt, &deliveredAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load shipment: %w", err)
	}
	shipment.Status = ShipmentStatus(status)
	shipment.ShippedAt = shippedAt
	shipment.DeliveredAt = deliveredAt
	packages, err := store.ListPackagesByFulfillment(ctx, fulfillmentID)
	if err != nil {
		return nil, err
	}
	shipment.Packages = packages
	events, err := store.listTrackingEvents(ctx, shipment.ShipmentID)
	if err != nil {
		return nil, err
	}
	shipment.TrackingEvents = events
	return &shipment, nil
}

func (store *PostgresStore) listTrackingEvents(ctx context.Context, shipmentID string) ([]TrackingEvent, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT tracking_event_id, shipment_id, status, description,
		       COALESCE(location, ''), occurred_at
		FROM tracking_event
		WHERE shipment_id = $1
		ORDER BY occurred_at, tracking_event_id`, shipmentID)
	if err != nil {
		return nil, fmt.Errorf("list tracking events: %w", err)
	}
	defer rows.Close()
	events := make([]TrackingEvent, 0)
	for rows.Next() {
		var event TrackingEvent
		var status string
		if err := rows.Scan(&event.TrackingEventID, &event.ShipmentID,
			&status, &event.Description, &event.Location, &event.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan tracking event: %w", err)
		}
		event.Status = ShipmentStatus(status)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tracking events: %w", err)
	}
	return events, nil
}

// SaveFulfill writes the per-location projection and the derived Outbox events
// inside one transaction. Conditional inserts (ON CONFLICT DO NOTHING) plus the
// UNIQUE(order_id, location_id) constraint make a duplicate replay a no-op that
// produces no new Outbox row.
func (store *PostgresStore) SaveFulfill(ctx context.Context, outcome FulfillOutcome) (FulfillResult, error) {
	if err := store.assert(); err != nil {
		return FulfillResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return FulfillResult{}, fmt.Errorf("begin fulfillment save: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := store.clock().UTC()
	insertedAny := false
	for _, order := range outcome.FulfillmentOrders {
		inserted, err := tx.Exec(ctx, `
			INSERT INTO fulfillment_order (
				fulfillment_id, order_id, location_id, status, created_at
			) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (order_id, location_id) DO NOTHING`,
			order.FulfillmentID, order.OrderID, order.LocationID,
			string(order.Status), canonicalCreatedAt(order.CreatedAt, now))
		if err != nil {
			return FulfillResult{}, fmt.Errorf("insert fulfillment order: %w", err)
		}
		if inserted.RowsAffected() == 0 {
			// A replay already created this fulfillment order. Skip the
			// dependent inserts; the original writer emitted the event.
			continue
		}
		insertedAny = true
		if err := saveFulfillmentChildren(ctx, tx, order); err != nil {
			return FulfillResult{}, err
		}
	}
	if insertedAny {
		for _, event := range outcome.Events {
			if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
				return FulfillResult{}, fmt.Errorf("insert fulfillment outbox: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FulfillResult{}, fmt.Errorf("commit fulfillment save: %w", err)
	}
	orders, err := store.GetFulfillmentsByOrder(ctx, outcome.OrderID)
	if err != nil {
		return FulfillResult{}, fmt.Errorf("reload fulfillment orders: %w", err)
	}
	result := FulfillResult{FulfillmentOrders: FulfillmentOrderList(orders)}
	if insertedAny {
		result.Events = append([]messaging.Event(nil), outcome.Events...)
	} else {
		result.Replayed = true
	}
	return result, nil
}

func saveFulfillmentChildren(ctx context.Context, tx pgx.Tx, order FulfillmentOrder) error {
	for _, item := range order.Items {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fulfillment_item (
				fulfillment_id, order_item_id, sku, quantity
			) VALUES ($1, $2, $3, $4)
			ON CONFLICT (fulfillment_id, order_item_id) DO NOTHING`,
			item.FulfillmentID, item.OrderItemID, item.SKU, item.Quantity); err != nil {
			return fmt.Errorf("insert fulfillment item: %w", err)
		}
	}
	for _, pick := range order.Picks {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pick_item (
				pick_item_id, fulfillment_id, order_item_id,
				requested_quantity, picked_quantity, status
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (fulfillment_id, order_item_id) DO NOTHING`,
			pick.PickItemID, pick.FulfillmentID, pick.OrderItemID,
			pick.RequestedQuantity, pick.PickedQuantity, string(pick.Status)); err != nil {
			return fmt.Errorf("insert pick item: %w", err)
		}
	}
	for _, pkg := range order.Packages {
		if _, err := tx.Exec(ctx, `
			INSERT INTO package (
				package_id, fulfillment_id, weight_grams,
				length_mm, width_mm, height_mm, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (package_id) DO NOTHING`,
			pkg.PackageID, pkg.FulfillmentID, pkg.WeightGrams,
			pkg.LengthMM, pkg.WidthMM, pkg.HeightMM, canonicalCreatedAt(order.CreatedAt, time.Time{})); err != nil {
			return fmt.Errorf("insert package: %w", err)
		}
		// Link package items. The default pack puts every fulfillment item into
		// the single package; a multi-package variant would loop differently.
		for _, item := range order.Items {
			if _, err := tx.Exec(ctx, `
				INSERT INTO package_item (package_id, fulfillment_id, order_item_id, quantity)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (package_id, order_item_id) DO NOTHING`,
				pkg.PackageID, pkg.FulfillmentID, item.OrderItemID, item.Quantity); err != nil {
				return fmt.Errorf("insert package item: %w", err)
			}
		}
	}
	if order.Shipment != nil {
		if err := saveShipment(ctx, tx, *order.Shipment, order); err != nil {
			return err
		}
	}
	return nil
}

func saveShipment(ctx context.Context, tx pgx.Tx, shipment Shipment, order FulfillmentOrder) error {
	var shippedAt, deliveredAt any
	if shipment.ShippedAt != nil {
		shippedAt = *shipment.ShippedAt
	}
	if shipment.DeliveredAt != nil {
		deliveredAt = *shipment.DeliveredAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO shipment (
			shipment_id, fulfillment_id, carrier, tracking_number,
			status, shipped_at, delivered_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (shipment_id) DO NOTHING`,
		shipment.ShipmentID, shipment.FulfillmentID, shipment.Carrier,
		shipment.TrackingNumber, string(shipment.Status), shippedAt, deliveredAt); err != nil {
		return fmt.Errorf("insert shipment: %w", err)
	}
	for _, pkg := range shipment.Packages {
		if _, err := tx.Exec(ctx, `
			INSERT INTO shipment_package (shipment_id, package_id, fulfillment_id)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`,
			shipment.ShipmentID, pkg.PackageID, shipment.FulfillmentID); err != nil {
			return fmt.Errorf("insert shipment package: %w", err)
		}
	}
	for _, event := range shipment.TrackingEvents {
		var location any
		if event.Location != "" {
			location = event.Location
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tracking_event (
				tracking_event_id, shipment_id, status, description, location, occurred_at
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tracking_event_id) DO NOTHING`,
			event.TrackingEventID, event.ShipmentID, string(event.Status),
			event.Description, location, event.OccurredAt); err != nil {
			return fmt.Errorf("insert tracking event: %w", err)
		}
	}
	return nil
}

// SaveCancel flips still-open fulfillment orders to 'cancelled' and writes the
// derived Outbox event in one transaction. Replays of the same idempotency key
// are a no-op: the Inbox on consumer-side records the inbound event_id, and a
// duplicate SaveCancel finds every row already 'cancelled' and emits no new
// Outbox row.
//
// The cancelled event fires whenever the order has any row that is NOT already
// 'cancelled' — including the case where every row is 'fulfilled'
// (terminal-delivered) and the UPDATE below flips zero rows. The order saga,
// after emitting fulfillment.cancel-requested.v1 for a paid order, requires
// fulfillment.cancelled.v1 to advance its compensation step; suppressing the
// event when shipments were already delivered would hang the saga forever. The
// existence check runs BEFORE the UPDATE so the open-row flips do not erase the
// signal; the UPDATE's state guard is preserved (already-terminal rows are not
// re-flipped).
func (store *PostgresStore) SaveCancel(ctx context.Context, outcome CancelOutcome) (CancelResult, error) {
	if err := store.assert(); err != nil {
		return CancelResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CancelResult{}, fmt.Errorf("begin fulfillment cancel: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Capture whether the order has any non-cancelled projection BEFORE the
	// UPDATE flips open rows. This is the emit predicate: true when any row is
	// open / in_progress / on_hold / fulfilled. Already-cancelled rows do not
	// count (a prior SaveCancel already emitted the event for this order).
	var hasUncancelled bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM fulfillment_order
			WHERE order_id = $1 AND status <> 'cancelled'
		)`, outcome.OrderID).Scan(&hasUncancelled); err != nil {
		return CancelResult{}, fmt.Errorf("check uncancelled fulfillment order: %w", err)
	}
	rows, err := tx.Query(ctx, `
		UPDATE fulfillment_order
		SET status = 'cancelled'
		WHERE order_id = $1 AND status NOT IN ('fulfilled', 'cancelled')
		RETURNING fulfillment_id, order_id, location_id, status, created_at`,
		outcome.OrderID)
	if err != nil {
		return CancelResult{}, fmt.Errorf("cancel fulfillment orders: %w", err)
	}
	cancelled := make(FulfillmentOrderList, 0)
	for rows.Next() {
		var order FulfillmentOrder
		if err := rows.Scan(&order.FulfillmentID, &order.OrderID, &order.LocationID,
			&order.Status, &order.CreatedAt); err != nil {
			rows.Close()
			return CancelResult{}, fmt.Errorf("scan cancelled order: %w", err)
		}
		cancelled = append(cancelled, order)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return CancelResult{}, fmt.Errorf("iterate cancelled orders: %w", err)
	}
	emitEvent := hasUncancelled && len(outcome.Events) > 0
	if emitEvent {
		for _, event := range outcome.Events {
			if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
				return CancelResult{}, fmt.Errorf("insert cancel outbox: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CancelResult{}, fmt.Errorf("commit fulfillment cancel: %w", err)
	}
	result := CancelResult{FulfillmentOrders: cancelled}
	if emitEvent {
		result.Events = append([]messaging.Event(nil), outcome.Events...)
	} else {
		result.Replayed = true
	}
	return result, nil
}

func canonicalCreatedAt(value, fallback time.Time) time.Time {
	if value.IsZero() {
		if fallback.IsZero() {
			return time.Now().UTC()
		}
		return fallback.UTC()
	}
	return value.UTC()
}

var _ Store = (*PostgresStore)(nil)
