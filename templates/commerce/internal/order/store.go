package order

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool  *pgxpool.Pool
	clock func() time.Time
}

func NewPostgresStore(pool *pgxpool.Pool, clock func() time.Time) *PostgresStore {
	if clock == nil {
		clock = time.Now
	}
	return &PostgresStore{pool: pool, clock: clock}
}

func (store *PostgresStore) CreateCart(ctx context.Context, cart Cart) (Cart, error) {
	if store == nil || store.pool == nil {
		return Cart{}, errors.New("order postgres store is unavailable")
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Cart{}, fmt.Errorf("begin create cart: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO cart (cart_id, customer_id, status, currency, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		cart.ID, cart.CustomerID, cart.Status, cart.Currency, cart.ExpiresAt.UTC()); err != nil {
		return Cart{}, fmt.Errorf("insert cart: %w", err)
	}
	version := cart.Version
	if version <= 0 {
		version = 1
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cart_revision (cart_id, version) VALUES ($1, $2)`,
		cart.ID, version); err != nil {
		return Cart{}, fmt.Errorf("insert cart revision: %w", err)
	}
	for _, line := range cart.Lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cart_item (cart_id, sku, quantity, unit_price_minor)
			VALUES ($1, $2, $3, $4)`,
			cart.ID, line.SKU, line.Quantity, line.UnitPriceMinor); err != nil {
			return Cart{}, fmt.Errorf("insert cart item: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Cart{}, fmt.Errorf("commit create cart: %w", err)
	}
	cart.Version = version
	return canonicalCart(cart), nil
}

func (store *PostgresStore) GetCart(ctx context.Context, id string) (Cart, error) {
	if store == nil || store.pool == nil {
		return Cart{}, errors.New("order postgres store is unavailable")
	}
	return getCart(ctx, store.pool, id)
}

type orderQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func getCart(ctx context.Context, queryer orderQueryer, id string) (Cart, error) {
	var cart Cart
	err := queryer.QueryRow(ctx, `
		SELECT c.cart_id, c.customer_id, c.status, c.currency, c.expires_at,
		       COALESCE(r.version, 1)
		FROM cart c
		LEFT JOIN cart_revision r USING (cart_id)
		WHERE c.cart_id = $1`, id).
		Scan(&cart.ID, &cart.CustomerID, &cart.Status, &cart.Currency, &cart.ExpiresAt, &cart.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, ErrCartNotFound
	}
	if err != nil {
		return Cart{}, fmt.Errorf("query cart: %w", err)
	}
	rows, err := queryer.Query(ctx, `
		SELECT sku, quantity, unit_price_minor
		FROM cart_item
		WHERE cart_id = $1
		ORDER BY sku`, id)
	if err != nil {
		return Cart{}, fmt.Errorf("query cart items: %w", err)
	}
	defer rows.Close()
	cart.Lines = []CartLine{}
	for rows.Next() {
		var line CartLine
		if err := rows.Scan(&line.SKU, &line.Quantity, &line.UnitPriceMinor); err != nil {
			return Cart{}, fmt.Errorf("scan cart item: %w", err)
		}
		cart.Lines = append(cart.Lines, line)
	}
	if err := rows.Err(); err != nil {
		return Cart{}, fmt.Errorf("iterate cart items: %w", err)
	}
	return canonicalCart(cart), nil
}

func (store *PostgresStore) MutateCart(ctx context.Context, mutation CartMutation) (Cart, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Cart{}, fmt.Errorf("begin cart mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO cart_revision (cart_id, version)
		SELECT cart_id, 1 FROM cart WHERE cart_id = $1
		ON CONFLICT (cart_id) DO NOTHING`, mutation.CartID); err != nil {
		return Cart{}, fmt.Errorf("initialize cart revision: %w", err)
	}
	var status CartStatus
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT status, expires_at FROM cart WHERE cart_id = $1 FOR UPDATE`,
		mutation.CartID).Scan(&status, &expiresAt); errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, ErrCartNotFound
	} else if err != nil {
		return Cart{}, fmt.Errorf("lock cart: %w", err)
	}
	if status != CartActive || !expiresAt.After(store.clock().UTC()) {
		return Cart{}, ErrInvalidCommand
	}
	command, err := tx.Exec(ctx, `
		UPDATE cart_revision
		SET version = version + 1
		WHERE cart_id = $1 AND version = $2`,
		mutation.CartID, mutation.ExpectedVersion)
	if err != nil {
		return Cart{}, fmt.Errorf("compare cart revision: %w", err)
	}
	if command.RowsAffected() != 1 {
		return Cart{}, ErrVersionConflict
	}
	switch mutation.Action {
	case CartAddLine:
		command, err = tx.Exec(ctx, `
			INSERT INTO cart_item (cart_id, sku, quantity, unit_price_minor)
			VALUES ($1, $2, $3, 0)
			ON CONFLICT (cart_id, sku) DO NOTHING`,
			mutation.CartID, mutation.SKU, mutation.Quantity)
	case CartChangeLine:
		command, err = tx.Exec(ctx, `
			UPDATE cart_item SET quantity = $3
			WHERE cart_id = $1 AND sku = $2`,
			mutation.CartID, mutation.SKU, mutation.Quantity)
	case CartRemoveLine:
		command, err = tx.Exec(ctx, `
			DELETE FROM cart_item WHERE cart_id = $1 AND sku = $2`,
			mutation.CartID, mutation.SKU)
	default:
		return Cart{}, ErrInvalidCommand
	}
	if err != nil {
		return Cart{}, fmt.Errorf("mutate cart item: %w", err)
	}
	if command.RowsAffected() != 1 {
		return Cart{}, ErrInvalidCommand
	}
	cart, err := getCart(ctx, tx, mutation.CartID)
	if err != nil {
		return Cart{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Cart{}, fmt.Errorf("commit cart mutation: %w", err)
	}
	return cart, nil
}

func (store *PostgresStore) FindCheckout(ctx context.Context, key string) (CheckoutRecord, bool, error) {
	var requestHash, orderID string
	err := store.pool.QueryRow(ctx, `
		SELECT request_hash, order_id
		FROM checkout_request
		WHERE idempotency_key = $1`, key).Scan(&requestHash, &orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckoutRecord{}, false, nil
	}
	if err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("query checkout request: %w", err)
	}
	order, err := getOrder(ctx, store.pool, orderID)
	if err != nil {
		return CheckoutRecord{}, false, err
	}
	return CheckoutRecord{RequestHash: requestHash, Order: order}, true, nil
}

func (store *PostgresStore) CommitCheckout(ctx context.Context, commit CheckoutCommit) (Order, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin checkout commit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('order:checkout:' || $1, 0))`,
		commit.Order.IdempotencyKey); err != nil {
		return Order{}, fmt.Errorf("lock checkout key: %w", err)
	}
	var requestHash, orderID string
	err = tx.QueryRow(ctx, `
		SELECT request_hash, order_id FROM checkout_request
		WHERE idempotency_key = $1`,
		commit.Order.IdempotencyKey).Scan(&requestHash, &orderID)
	if err == nil {
		if requestHash != commit.RequestHash {
			return Order{}, ErrIdempotencyConflict
		}
		order, err := getOrder(ctx, tx, orderID)
		if err != nil {
			return Order{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Order{}, fmt.Errorf("commit checkout replay: %w", err)
		}
		order.Replayed = true
		return order, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("query checkout replay: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cart_revision (cart_id, version)
		SELECT cart_id, 1 FROM cart WHERE cart_id = $1
		ON CONFLICT (cart_id) DO NOTHING`, commit.CartID); err != nil {
		return Order{}, fmt.Errorf("initialize checkout cart revision: %w", err)
	}
	var status CartStatus
	var version int64
	if err := tx.QueryRow(ctx, `
		SELECT c.status, r.version
		FROM cart c JOIN cart_revision r USING (cart_id)
		WHERE c.cart_id = $1
		FOR UPDATE OF c, r`, commit.CartID).Scan(&status, &version); errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrCartNotFound
	} else if err != nil {
		return Order{}, fmt.Errorf("lock checkout cart: %w", err)
	}
	if status != CartActive || version != commit.CartVersion {
		return Order{}, ErrVersionConflict
	}
	order := canonicalOrder(commit.Order)
	if err := insertOrder(ctx, tx, order); err != nil {
		if uniqueViolation(err) {
			return Order{}, ErrIdempotencyConflict
		}
		return Order{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO checkout_request (
			idempotency_key, request_hash, order_id, created_at
		) VALUES ($1, $2, $3, $4)`,
		order.IdempotencyKey, commit.RequestHash, order.OrderID, order.PlacedAt); err != nil {
		if uniqueViolation(err) {
			return Order{}, ErrIdempotencyConflict
		}
		return Order{}, fmt.Errorf("insert checkout request: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cart SET status = 'converted'
		WHERE cart_id = $1 AND status = 'active'`, commit.CartID); err != nil {
		return Order{}, fmt.Errorf("convert checkout cart: %w", err)
	}
	if err := messaging.InsertOutbox(ctx, tx, commit.Event); err != nil {
		return Order{}, fmt.Errorf("insert checkout outbox: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit checkout: %w", err)
	}
	return order, nil
}

func insertOrder(ctx context.Context, tx pgx.Tx, order Order) error {
	if err := reconcileStoredOrder(order); err != nil {
		return err
	}
	address := order.ShippingAddress
	if len(address) == 0 || !json.Valid(address) {
		return ErrInvalidCommand
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sales_order (
			order_id, order_no, customer_id, status, payment_status,
			fulfillment_status, currency, subtotal_minor, discount_minor,
			shipping_minor, tax_minor, total_minor, shipping_address,
			idempotency_key, placed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
		)`,
		order.OrderID, order.Number, order.CustomerID, order.Status, order.PaymentStatus,
		order.FulfillmentStatus, order.Currency, order.SubtotalMinor, order.DiscountMinor,
		order.ShippingMinor, order.TaxMinor, order.TotalMinor, address,
		order.IdempotencyKey, order.PlacedAt); err != nil {
		return fmt.Errorf("insert sales order: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_customer_snapshot (
			order_id, email, name, phone, billing_address
		) VALUES ($1, $2, $3, NULLIF($4, ''), NULL)`,
		order.OrderID, order.Customer.Email, order.Customer.Name, order.Customer.Phone); err != nil {
		return fmt.Errorf("insert order customer snapshot: %w", err)
	}
	for _, line := range order.Lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_item (
				order_item_id, order_id, sku, title, quantity,
				unit_price_minor, discount_minor, total_minor
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			line.ID, order.OrderID, line.SKU, line.Title, line.Quantity,
			line.UnitPriceMinor, line.DiscountMinor, line.TotalMinor); err != nil {
			return fmt.Errorf("insert order item: %w", err)
		}
		if line.DiscountMinor > 0 {
			allocationID := deterministicChildID("ALLOC", order.OrderID, line.ID, 0)
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_discount_allocation (
					allocation_id, order_id, order_item_id, source, amount_minor
				) VALUES ($1, $2, $3, 'checkout', $4)`,
				allocationID, order.OrderID, line.ID, line.DiscountMinor); err != nil {
				return fmt.Errorf("insert order discount allocation: %w", err)
			}
		}
	}
	historyID := deterministicChildID("HIST", order.OrderID, "placed", 0)
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_status_history (
			event_id, order_id, from_status, to_status, reason, occurred_at
		) VALUES ($1, $2, NULL, 'pending', 'checkout', $3)`,
		historyID, order.OrderID, order.PlacedAt); err != nil {
		return fmt.Errorf("insert order status history: %w", err)
	}
	sagaID := deterministicChildID("SAGA", order.OrderID, "checkout", 0)
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_saga (
			saga_id, order_id, state, version, created_at, updated_at
		) VALUES ($1, $2, 'paying', 0, $3, $3)`,
		sagaID, order.OrderID, order.PlacedAt); err != nil {
		return fmt.Errorf("insert order saga: %w", err)
	}
	for _, step := range []struct {
		name   string
		status string
	}{
		{name: "customer_validated", status: "completed"},
		{name: "catalog_snapshotted", status: "completed"},
		{name: "inventory_reserved", status: "completed"},
		{name: "payment_requested", status: "pending"},
	} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_saga_step (
				saga_id, step, status, updated_at
			) VALUES ($1, $2, $3, $4)`,
			sagaID, step.name, step.status, order.PlacedAt); err != nil {
			return fmt.Errorf("insert order saga step %s: %w", step.name, err)
		}
	}
	return nil
}

func reconcileStoredOrder(order Order) error {
	lines := make([]Line, len(order.Lines))
	for index, line := range order.Lines {
		lines[index] = Line{
			Quantity: line.Quantity, UnitPriceMinor: line.UnitPriceMinor,
			DiscountMinor: line.DiscountMinor,
		}
	}
	totals, err := CalculateTotals(lines, order.ShippingMinor, order.TaxMinor)
	if err != nil {
		return err
	}
	if totals.Subtotal != order.SubtotalMinor || totals.Discount != order.DiscountMinor ||
		totals.Total != order.TotalMinor {
		return ErrInvalidMoney
	}
	return reconcileOrderAmounts(order.Lines, totals)
}

func (store *PostgresStore) ListOrders(ctx context.Context, cursor OrderCursor, limit int) ([]Order, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if cursor.PlacedAt.IsZero() {
		rows, err = store.pool.Query(ctx, `
			SELECT order_id
			FROM sales_order
			ORDER BY placed_at DESC, order_id DESC
			LIMIT $1`, limit)
	} else {
		rows, err = store.pool.Query(ctx, `
			SELECT order_id
			FROM sales_order
			WHERE (placed_at, order_id) < ($1, $2)
			ORDER BY placed_at DESC, order_id DESC
			LIMIT $3`, cursor.PlacedAt, cursor.OrderID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query order page: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan order page: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate order page: %w", err)
	}
	rows.Close()
	orders := make([]Order, 0, len(ids))
	for _, id := range ids {
		order, err := getOrder(ctx, store.pool, id)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, nil
}

func (store *PostgresStore) GetOrder(ctx context.Context, id string) (Order, error) {
	return getOrder(ctx, store.pool, id)
}

func getOrder(ctx context.Context, queryer orderQueryer, id string) (Order, error) {
	var order Order
	var address []byte
	var billingAddress []byte
	err := queryer.QueryRow(ctx, `
		SELECT o.order_id, o.order_no, o.customer_id, o.status,
		       o.payment_status, o.fulfillment_status, o.currency,
		       o.subtotal_minor, o.discount_minor, o.shipping_minor,
		       o.tax_minor, o.total_minor, o.shipping_address,
		       o.idempotency_key, o.placed_at,
		       COALESCE(c.email, ''), COALESCE(c.name, ''), COALESCE(c.phone, ''),
		       COALESCE(c.billing_address, 'null'::jsonb),
		       COALESCE(s.state, '')
		FROM sales_order o
		LEFT JOIN order_customer_snapshot c USING (order_id)
		LEFT JOIN order_saga s USING (order_id)
		WHERE o.order_id = $1`, id).
		Scan(&order.OrderID, &order.Number, &order.CustomerID, &order.Status,
			&order.PaymentStatus, &order.FulfillmentStatus, &order.Currency,
			&order.SubtotalMinor, &order.DiscountMinor, &order.ShippingMinor,
			&order.TaxMinor, &order.TotalMinor, &address,
			&order.IdempotencyKey, &order.PlacedAt,
			&order.Customer.Email, &order.Customer.Name, &order.Customer.Phone,
			&billingAddress, &order.SagaState)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrOrderNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("query order: %w", err)
	}
	order.Customer.ID = order.CustomerID
	order.ShippingAddress = append(json.RawMessage(nil), address...)
	order.Customer.Address = append(json.RawMessage(nil), address...)
	rows, err := queryer.Query(ctx, `
		SELECT order_item_id, sku, title, quantity, unit_price_minor,
		       discount_minor, total_minor
		FROM order_item
		WHERE order_id = $1
		ORDER BY order_item_id`, id)
	if err != nil {
		return Order{}, fmt.Errorf("query order items: %w", err)
	}
	defer rows.Close()
	order.Lines = []OrderLine{}
	for rows.Next() {
		var line OrderLine
		if err := rows.Scan(&line.ID, &line.SKU, &line.Title, &line.Quantity,
			&line.UnitPriceMinor, &line.DiscountMinor, &line.TotalMinor); err != nil {
			return Order{}, fmt.Errorf("scan order item: %w", err)
		}
		order.Lines = append(order.Lines, line)
	}
	if err := rows.Err(); err != nil {
		return Order{}, fmt.Errorf("iterate order items: %w", err)
	}
	return canonicalOrder(order), nil
}

func (store *PostgresStore) CancelOrder(
	ctx context.Context,
	command CancelCommand,
	events []messaging.Event,
) (Order, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Order{}, fmt.Errorf("begin cancel order: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	order, err := getOrderForUpdate(ctx, tx, command.OrderID)
	if err != nil {
		return Order{}, err
	}
	if order.Status == "cancelled" {
		if err := tx.Commit(ctx); err != nil {
			return Order{}, fmt.Errorf("commit cancellation replay: %w", err)
		}
		return order, nil
	}
	now := store.clock().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE sales_order SET status = 'cancelled'
		WHERE order_id = $1 AND status IN ('pending', 'confirmed')`,
		command.OrderID); err != nil {
		return Order{}, fmt.Errorf("cancel sales order: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga
		SET state = 'compensating', version = version + 1,
		    updated_at = $2
		WHERE order_id = $1 AND state <> 'failed'`,
		command.OrderID, now); err != nil {
		return Order{}, fmt.Errorf("advance cancellation saga: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'compensated', updated_at = $2
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'inventory_reserved' AND status = 'completed'`,
		command.OrderID, now); err != nil {
		return Order{}, fmt.Errorf("compensate inventory saga step: %w", err)
	}
	if order.PaymentStatus == "paid" || order.PaymentStatus == "authorized" ||
		order.PaymentStatus == "partially_refunded" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_saga_step (saga_id, step, status, updated_at)
			SELECT saga_id, 'refund_requested', 'pending', $2
			FROM order_saga WHERE order_id = $1
			ON CONFLICT (saga_id, step) DO NOTHING`,
			command.OrderID, now); err != nil {
			return Order{}, fmt.Errorf("create refund saga step: %w", err)
		}
	}
	historyID := deterministicChildID("HIST", command.OrderID, "cancel:"+command.Reason, int(now.UnixNano()))
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_status_history (
			event_id, order_id, from_status, to_status, reason, occurred_at
		) VALUES ($1, $2, $3, 'cancelled', $4, $5)`,
		historyID, command.OrderID, order.Status, command.Reason, now); err != nil {
		return Order{}, fmt.Errorf("insert cancellation history: %w", err)
	}
	for _, event := range events {
		if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
			return Order{}, fmt.Errorf("insert cancellation outbox: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit cancellation: %w", err)
	}
	return store.GetOrder(ctx, command.OrderID)
}

func getOrderForUpdate(ctx context.Context, tx pgx.Tx, id string) (Order, error) {
	var order Order
	err := tx.QueryRow(ctx, `
		SELECT order_id, status, payment_status, fulfillment_status, total_minor
		FROM sales_order WHERE order_id = $1 FOR UPDATE`, id).
		Scan(&order.OrderID, &order.Status, &order.PaymentStatus,
			&order.FulfillmentStatus, &order.TotalMinor)
	if errors.Is(err, pgx.ErrNoRows) {
		return Order{}, ErrOrderNotFound
	}
	if err != nil {
		return Order{}, fmt.Errorf("lock order: %w", err)
	}
	return order, nil
}

func (store *PostgresStore) HandleEvent(ctx context.Context, event messaging.Event) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin order event: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := messaging.HandleOnce(ctx, tx, "order-saga", event, func() error {
		return store.applyEvent(ctx, tx, event)
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit order event: %w", err)
	}
	return nil
}

func (store *PostgresStore) applyEvent(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	switch event.Type {
	case "payment.failed.v1":
		return store.applyPaymentFailed(ctx, tx, event)
	case "payment.captured.v1", "payment.succeeded.v1", "payment.paid.v1":
		return store.applyPaymentPaid(ctx, tx, event)
	case "fulfillment.completed.v1", "fulfillment.delivered.v1":
		return store.applyFulfillmentCompleted(ctx, tx, event)
	case "payment.refunded.v1", "refund.succeeded.v1":
		return store.applyRefunded(ctx, tx, event)
	default:
		return messaging.NonRetryable(fmt.Errorf("unsupported order event type %s", event.Type))
	}
}

func (store *PostgresStore) applyPaymentFailed(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "pending" || order.PaymentStatus != "pending" {
		return nil
	}
	now := event.OccurredAt.UTC()
	command, err := tx.Exec(ctx, `
		UPDATE sales_order
		SET status = 'cancelled', payment_status = 'failed'
		WHERE order_id = $1 AND status = 'pending' AND payment_status = 'pending'`,
		event.Subject)
	if err != nil {
		return fmt.Errorf("apply payment failure: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil
	}
	if err := recordStatusTransition(ctx, tx, event, "pending", "cancelled"); err != nil {
		return err
	}
	if err := updateSagaForPayment(ctx, tx, event, "failed", "failed", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'compensated', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'inventory_reserved' AND status = 'completed'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("compensate inventory saga step: %w", err)
	}
	return insertDerivedEvents(ctx, tx, event, order,
		"inventory.release-requested.v1", "order.cancelled.v1")
}

func (store *PostgresStore) applyPaymentPaid(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "pending" || order.PaymentStatus != "pending" {
		return nil
	}
	now := event.OccurredAt.UTC()
	command, err := tx.Exec(ctx, `
		UPDATE sales_order
		SET status = 'confirmed', payment_status = 'paid'
		WHERE order_id = $1 AND status = 'pending' AND payment_status = 'pending'`,
		event.Subject)
	if err != nil {
		return fmt.Errorf("apply paid payment: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil
	}
	if err := recordStatusTransition(ctx, tx, event, "pending", "confirmed"); err != nil {
		return err
	}
	if err := updateSagaForPayment(ctx, tx, event, "completed", "completed", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_saga_step (saga_id, step, status, updated_at)
		SELECT saga_id, 'fulfillment_requested', 'pending', $2
		FROM order_saga WHERE order_id = $1
		ON CONFLICT (saga_id, step) DO NOTHING`,
		event.Subject, now); err != nil {
		return fmt.Errorf("create fulfillment saga step: %w", err)
	}
	return insertDerivedEvents(ctx, tx, event, order,
		"inventory.commit-requested.v1", "order.paid.v1")
}

func updateSagaForPayment(
	ctx context.Context,
	tx pgx.Tx,
	event messaging.Event,
	sagaState string,
	stepStatus string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga
		SET state = $2, version = version + 1, last_event_id = $3::uuid,
		    updated_at = $4
		WHERE order_id = $1 AND state = 'paying'`,
		event.Subject, sagaState, event.ID, now); err != nil {
		return fmt.Errorf("advance payment saga: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = $2, event_id = $3::uuid, updated_at = $4
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'payment_requested' AND status = 'pending'`,
		event.Subject, stepStatus, event.ID, now); err != nil {
		return fmt.Errorf("advance payment saga step: %w", err)
	}
	return nil
}

func (store *PostgresStore) applyFulfillmentCompleted(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	command, err := tx.Exec(ctx, `
		UPDATE sales_order
		SET status = 'completed', fulfillment_status = 'fulfilled'
		WHERE order_id = $1 AND status = 'confirmed'
		  AND payment_status IN ('paid', 'partially_refunded', 'refunded')
		  AND fulfillment_status <> 'fulfilled'`,
		event.Subject)
	if err != nil {
		return fmt.Errorf("apply fulfillment completion: %w", err)
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	if err := recordStatusTransition(ctx, tx, event, "confirmed", "completed"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'completed', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'fulfillment_requested' AND status = 'pending'`,
		event.Subject, event.ID, event.OccurredAt.UTC()); err != nil {
		return fmt.Errorf("complete fulfillment saga step: %w", err)
	}
	return nil
}

func (store *PostgresStore) applyRefunded(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	command, err := tx.Exec(ctx, `
		UPDATE sales_order
		SET payment_status = 'refunded'
		WHERE order_id = $1 AND status = 'cancelled'
		  AND payment_status IN ('paid', 'partially_refunded')`,
		event.Subject)
	if err != nil {
		return fmt.Errorf("apply refund: %w", err)
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	now := event.OccurredAt.UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga
		SET state = 'failed', version = version + 1,
		    last_event_id = $2::uuid, updated_at = $3
		WHERE order_id = $1 AND state = 'compensating'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete refund saga: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'completed', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'refund_requested' AND status = 'pending'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete refund saga step: %w", err)
	}
	return nil
}

func insertDerivedEvents(
	ctx context.Context,
	tx pgx.Tx,
	cause messaging.Event,
	order Order,
	kinds ...string,
) error {
	for _, kind := range kinds {
		event := newOrderEvent(kind, order, cause.CorrelationID, cause.ID,
			map[string]any{"order_id": cause.Subject}, cause.OccurredAt.UTC())
		if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
			return fmt.Errorf("insert %s outbox: %w", kind, err)
		}
	}
	return nil
}

func recordStatusTransition(
	ctx context.Context,
	tx pgx.Tx,
	event messaging.Event,
	from string,
	to string,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_status_history (
			event_id, order_id, from_status, to_status, reason, occurred_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		event.ID, event.Subject, from, to, event.Type, event.OccurredAt.UTC()); err != nil {
		return fmt.Errorf("insert event status history: %w", err)
	}
	return nil
}

func uniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

var _ Store = (*PostgresStore)(nil)
