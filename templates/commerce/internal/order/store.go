package order

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
		SELECT pg_advisory_xact_lock(hashtextextended('order:cart:create:' || $1, 0))`,
		cart.IdempotencyKey); err != nil {
		return Cart{}, fmt.Errorf("lock create cart key: %w", err)
	}
	hash := commandFingerprint(struct {
		CustomerID string `json:"customer_id"`
		Currency   string `json:"currency"`
	}{cart.CustomerID, cart.Currency})
	var priorHash string
	var priorResponse []byte
	err = tx.QueryRow(ctx, `
		SELECT request_hash, response
		FROM order_command
		WHERE command_scope = 'cart:create' AND idempotency_key = $1`,
		cart.IdempotencyKey).Scan(&priorHash, &priorResponse)
	if err == nil {
		if priorHash != hash {
			return Cart{}, ErrIdempotencyConflict
		}
		var replay Cart
		if err := json.Unmarshal(priorResponse, &replay); err != nil {
			return Cart{}, fmt.Errorf("decode create cart replay: %w", err)
		}
		replay.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return Cart{}, fmt.Errorf("commit create cart replay: %w", err)
		}
		return replay, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, fmt.Errorf("query create cart replay: %w", err)
	}
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
	cart.Version = version
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_command (
			command_scope, idempotency_key, request_hash, resource_id, response, created_at
		) VALUES ('cart:create', $1, $2, $3, $4, $5)`,
		cart.IdempotencyKey, hash, cart.ID, mustJSON(canonicalCart(cart)), store.clock().UTC()); err != nil {
		if uniqueViolation(err) {
			return Cart{}, ErrIdempotencyConflict
		}
		return Cart{}, fmt.Errorf("insert create cart replay: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Cart{}, fmt.Errorf("commit create cart: %w", err)
	}
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
	scope := "cart:mutate:" + mutation.CartID
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('order:' || $1 || ':' || $2, 0))`,
		scope, mutation.IdempotencyKey); err != nil {
		return Cart{}, fmt.Errorf("lock cart mutation key: %w", err)
	}
	hash := commandFingerprint(struct {
		CartID          string             `json:"cart_id"`
		SKU             string             `json:"sku"`
		Quantity        int64              `json:"quantity"`
		ExpectedVersion int64              `json:"expected_version"`
		Action          CartMutationAction `json:"action"`
	}{mutation.CartID, mutation.SKU, mutation.Quantity, mutation.ExpectedVersion, mutation.Action})
	var priorHash string
	var priorResponse []byte
	err = tx.QueryRow(ctx, `
		SELECT request_hash, response FROM order_command
		WHERE command_scope = $1 AND idempotency_key = $2`,
		scope, mutation.IdempotencyKey).Scan(&priorHash, &priorResponse)
	if err == nil {
		if priorHash != hash {
			return Cart{}, ErrIdempotencyConflict
		}
		var replay Cart
		if err := json.Unmarshal(priorResponse, &replay); err != nil {
			return Cart{}, fmt.Errorf("decode cart mutation replay: %w", err)
		}
		replay.Replayed = true
		if err := tx.Commit(ctx); err != nil {
			return Cart{}, fmt.Errorf("commit cart mutation replay: %w", err)
		}
		return replay, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Cart{}, fmt.Errorf("query cart mutation replay: %w", err)
	}
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
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_command (
			command_scope, idempotency_key, request_hash, resource_id, response, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		scope, mutation.IdempotencyKey, hash, mutation.CartID,
		mustJSON(canonicalCart(cart)), store.clock().UTC()); err != nil {
		if uniqueViolation(err) {
			return Cart{}, ErrIdempotencyConflict
		}
		return Cart{}, fmt.Errorf("insert cart mutation replay: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Cart{}, fmt.Errorf("commit cart mutation: %w", err)
	}
	return cart, nil
}

func (store *PostgresStore) FindCheckout(ctx context.Context, key string) (CheckoutRecord, bool, error) {
	var requestHash, phase, orderID string
	var preparedOrder, preparedReservation, reservationResult []byte
	err := store.pool.QueryRow(ctx, `
		SELECT request_hash, phase, order_id,
		       COALESCE(prepared_order, 'null'::jsonb),
		       COALESCE(prepared_reservation, 'null'::jsonb),
		       COALESCE(reservation_result, 'null'::jsonb)
		FROM checkout_request
		WHERE idempotency_key = $1`, key).Scan(
		&requestHash, &phase, &orderID, &preparedOrder, &preparedReservation, &reservationResult)
	if errors.Is(err, pgx.ErrNoRows) {
		return CheckoutRecord{}, false, nil
	}
	if err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("query checkout request: %w", err)
	}
	record := CheckoutRecord{RequestHash: requestHash, Phase: phase}
	if string(preparedOrder) != "null" {
		if err := json.Unmarshal(preparedOrder, &record.PreparedOrder); err != nil {
			return CheckoutRecord{}, false, fmt.Errorf("decode prepared checkout order: %w", err)
		}
	}
	if string(preparedReservation) != "null" {
		if err := json.Unmarshal(preparedReservation, &record.PreparedReservation); err != nil {
			return CheckoutRecord{}, false, fmt.Errorf("decode prepared reservation: %w", err)
		}
	}
	if string(reservationResult) != "null" {
		if err := json.Unmarshal(reservationResult, &record.ReservationResult); err != nil {
			return CheckoutRecord{}, false, fmt.Errorf("decode reservation result: %w", err)
		}
	}
	if phase == "committed" {
		order, err := getOrder(ctx, store.pool, orderID)
		if err != nil {
			return CheckoutRecord{}, false, err
		}
		record.Order = order
	}
	return record, true, nil
}

func (store *PostgresStore) ClaimCheckout(
	ctx context.Context,
	claim CheckoutClaim,
) (CheckoutRecord, bool, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("begin checkout claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('order:checkout:' || $1, 0))`,
		claim.IdempotencyKey); err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("lock checkout claim key: %w", err)
	}
	var existingHash, existingPhase string
	var existingLease time.Time
	err = tx.QueryRow(ctx, `
		SELECT request_hash, phase, lease_until
		FROM checkout_request
		WHERE idempotency_key = $1
		FOR UPDATE`, claim.IdempotencyKey).Scan(&existingHash, &existingPhase, &existingLease)
	if err == nil {
		if existingHash != claim.RequestHash {
			return CheckoutRecord{}, false, ErrIdempotencyConflict
		}
		owned := false
		if existingPhase != "committed" && existingPhase != "failed" &&
			existingPhase != "compensation_needed" && !existingLease.After(claim.Now) {
			command, err := tx.Exec(ctx, `
				UPDATE checkout_request
				SET lease_until = $3, updated_at = $2
				WHERE idempotency_key = $1 AND request_hash = $4
				  AND phase NOT IN ('committed', 'failed', 'compensation_needed')
				  AND lease_until <= $2`,
				claim.IdempotencyKey, claim.Now, claim.Now.Add(30*time.Second), claim.RequestHash)
			if err != nil {
				return CheckoutRecord{}, false, fmt.Errorf("renew checkout claim: %w", err)
			}
			owned = command.RowsAffected() == 1
		}
		if err := tx.Commit(ctx); err != nil {
			return CheckoutRecord{}, false, fmt.Errorf("commit existing checkout claim: %w", err)
		}
		record, found, err := store.FindCheckout(ctx, claim.IdempotencyKey)
		if err != nil || !found {
			return CheckoutRecord{}, false, err
		}
		return record, owned, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CheckoutRecord{}, false, fmt.Errorf("query existing checkout claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cart_revision (cart_id, version)
		SELECT cart_id, 1 FROM cart WHERE cart_id = $1
		ON CONFLICT (cart_id) DO NOTHING`, claim.Cart.ID); err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("initialize checkout claim cart revision: %w", err)
	}
	var status CartStatus
	var version int64
	if err := tx.QueryRow(ctx, `
		SELECT c.status, r.version
		FROM cart c JOIN cart_revision r USING (cart_id)
		WHERE c.cart_id = $1
		FOR UPDATE OF c, r`, claim.Cart.ID).Scan(&status, &version); errors.Is(err, pgx.ErrNoRows) {
		return CheckoutRecord{}, false, ErrCartNotFound
	} else if err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("lock checkout cart for claim: %w", err)
	}
	if status != CartActive || version != claim.Cart.Version {
		return CheckoutRecord{}, false, ErrVersionConflict
	}
	result, err := tx.Exec(ctx, `
		INSERT INTO checkout_request (
			idempotency_key, request_hash, phase, cart_id, cart_version,
			order_id, lease_until, created_at, updated_at
		) VALUES ($1, $2, 'claimed', $3, $4, $5, $6, $7, $7)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		claim.IdempotencyKey, claim.RequestHash, claim.Cart.ID, claim.Cart.Version,
		claim.OrderID, claim.Now.Add(30*time.Second), claim.Now)
	if err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("insert checkout claim: %w", err)
	}
	owned := result.RowsAffected() == 1
	var requestHash, phase string
	var leaseUntil time.Time
	if err := tx.QueryRow(ctx, `
		SELECT request_hash, phase, lease_until
		FROM checkout_request
		WHERE idempotency_key = $1
		FOR UPDATE`, claim.IdempotencyKey).Scan(&requestHash, &phase, &leaseUntil); err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("read checkout claim: %w", err)
	}
	if requestHash != claim.RequestHash {
		return CheckoutRecord{}, false, ErrIdempotencyConflict
	}
	if !owned && phase != "committed" && !leaseUntil.After(claim.Now) {
		command, err := tx.Exec(ctx, `
			UPDATE checkout_request
			SET lease_until = $3, updated_at = $2
			WHERE idempotency_key = $1 AND request_hash = $4
			  AND phase NOT IN ('committed', 'failed', 'compensation_needed')
			  AND lease_until <= $2`,
			claim.IdempotencyKey, claim.Now, claim.Now.Add(30*time.Second), claim.RequestHash)
		if err != nil {
			return CheckoutRecord{}, false, fmt.Errorf("renew checkout claim: %w", err)
		}
		owned = command.RowsAffected() == 1
	}
	if err := tx.Commit(ctx); err != nil {
		return CheckoutRecord{}, false, fmt.Errorf("commit checkout claim: %w", err)
	}
	record, found, err := store.FindCheckout(ctx, claim.IdempotencyKey)
	if err != nil || !found {
		return CheckoutRecord{}, false, err
	}
	return record, owned, nil
}

func (store *PostgresStore) SaveCheckoutPrepared(
	ctx context.Context,
	key string,
	hash string,
	order Order,
	reservation ReservationCommand,
) error {
	orderJSON, err := json.Marshal(order)
	if err != nil {
		return err
	}
	reservationJSON, err := json.Marshal(reservation)
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE checkout_request
		SET phase = 'prepared', prepared_order = $3, prepared_reservation = $4,
		    updated_at = $5
		WHERE idempotency_key = $1 AND request_hash = $2 AND phase = 'claimed'`,
		key, hash, orderJSON, reservationJSON, store.clock().UTC())
	if err != nil {
		return fmt.Errorf("save prepared checkout: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCheckoutUncertain
	}
	return nil
}

func (store *PostgresStore) SaveCheckoutReserved(
	ctx context.Context,
	key string,
	hash string,
	result ReservationResult,
) error {
	body, err := json.Marshal(result)
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE checkout_request
		SET phase = 'reserved', reservation_result = $3, updated_at = $4
		WHERE idempotency_key = $1 AND request_hash = $2 AND phase = 'prepared'`,
		key, hash, body, store.clock().UTC())
	if err != nil {
		return fmt.Errorf("save reserved checkout: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCheckoutUncertain
	}
	return nil
}

func (store *PostgresStore) FailCheckout(
	ctx context.Context,
	key string,
	hash string,
	code string,
) error {
	if code == "retryable" {
		command, err := store.pool.Exec(ctx, `
			UPDATE checkout_request
			SET lease_until = $3, failure_code = $4, updated_at = $3
			WHERE idempotency_key = $1 AND request_hash = $2
			  AND phase NOT IN ('committed', 'failed', 'compensation_needed')`,
			key, hash, store.clock().UTC(), code)
		if err != nil {
			return fmt.Errorf("release retryable checkout claim: %w", err)
		}
		if command.RowsAffected() != 1 {
			return ErrCheckoutUncertain
		}
		return nil
	}
	phase := "failed"
	if code == "compensation_needed" {
		phase = "compensation_needed"
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE checkout_request
		SET phase = $3, failure_code = $4, updated_at = $5
		WHERE idempotency_key = $1 AND request_hash = $2 AND phase <> 'committed'`,
		key, hash, phase, code, store.clock().UTC())
	if err != nil {
		return fmt.Errorf("fail checkout: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrCheckoutUncertain
	}
	return nil
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
	var requestHash, orderID, phase string
	err = tx.QueryRow(ctx, `
		SELECT request_hash, order_id, phase FROM checkout_request
		WHERE idempotency_key = $1`,
		commit.Order.IdempotencyKey).Scan(&requestHash, &orderID, &phase)
	if err == nil {
		if requestHash != commit.RequestHash {
			return Order{}, ErrIdempotencyConflict
		}
		if phase == "committed" {
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
		if phase != "reserved" {
			return Order{}, ErrCheckoutUncertain
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
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
	var reservationBody []byte
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(reservation_result, '{"order_id":"","allocations":[]}'::jsonb)
		FROM checkout_request WHERE idempotency_key = $1`,
		order.IdempotencyKey).Scan(&reservationBody); err == nil {
		var reservation ReservationResult
		if err := json.Unmarshal(reservationBody, &reservation); err != nil {
			return Order{}, fmt.Errorf("decode committed reservation: %w", err)
		}
		for _, allocation := range reservation.Allocations {
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_inventory_allocation (
					order_id, allocation_id, sku, quantity, status
				) VALUES ($1, $2, $3, $4, $5)`,
				order.OrderID, allocation.AllocationID, allocation.SKU,
				allocation.Quantity, allocation.Status); err != nil {
				return Order{}, fmt.Errorf("insert inventory allocation snapshot: %w", err)
			}
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO checkout_request (
				idempotency_key, request_hash, phase, cart_id, cart_version,
				order_id, prepared_order, prepared_reservation,
				lease_until, created_at, updated_at
			) VALUES ($1, $2, 'reserved', $3, $4, $5, $6, '{}'::jsonb, $7, $7, $7)`,
			order.IdempotencyKey, commit.RequestHash, commit.CartID, commit.CartVersion,
			order.OrderID, mustJSON(order), order.PlacedAt); err != nil {
			return Order{}, fmt.Errorf("insert legacy checkout request: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE cart SET status = 'converted'
		WHERE cart_id = $1 AND status = 'active'`, commit.CartID); err != nil {
		return Order{}, fmt.Errorf("convert checkout cart: %w", err)
	}
	if err := messaging.InsertOutbox(ctx, tx, commit.Event); err != nil {
		return Order{}, fmt.Errorf("insert checkout outbox: %w", err)
	}
	command, err := tx.Exec(ctx, `
		UPDATE checkout_request
		SET phase = 'committed', updated_at = $3
		WHERE idempotency_key = $1 AND request_hash = $2 AND phase = 'reserved'`,
		order.IdempotencyKey, commit.RequestHash, store.clock().UTC())
	if err != nil {
		return Order{}, fmt.Errorf("complete checkout request: %w", err)
	}
	if command.RowsAffected() != 1 {
		return Order{}, ErrCheckoutUncertain
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
	var jurisdictionAddress struct {
		CountryCode string `json:"country_code"`
		Province    string `json:"province"`
		Region      string `json:"region"`
	}
	_ = json.Unmarshal(address, &jurisdictionAddress)
	jurisdiction := strings.Trim(strings.Join([]string{
		jurisdictionAddress.CountryCode,
		defaultText(jurisdictionAddress.Province, jurisdictionAddress.Region),
	}, ":"), ":")
	if jurisdiction == "" {
		jurisdiction = "shipping_address"
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
		attributes := line.Attributes
		if attributes == nil {
			attributes = map[string]any{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO order_item_snapshot (
				order_item_id, product_id, product_title, variant_title,
				attributes, weight_grams, channel, currency
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			line.ID, defaultText(line.ProductID, line.SKU),
			defaultText(line.ProductTitle, line.Title),
			defaultText(line.VariantTitle, line.Title), mustJSON(attributes),
			line.WeightGrams, defaultText(line.Channel, "legacy"), order.Currency); err != nil {
			return fmt.Errorf("insert order item snapshot: %w", err)
		}
		if line.DiscountMinor > 0 {
			allocationID := deterministicChildID("ALLOC", order.OrderID, line.ID, 0)
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_discount_allocation (
					allocation_id, order_id, order_item_id, source, amount_minor
				) VALUES ($1, $2, $3, $4, $5)`,
				allocationID, order.OrderID, line.ID,
				defaultText(order.CouponCode, "checkout"), line.DiscountMinor); err != nil {
				return fmt.Errorf("insert order discount allocation: %w", err)
			}
		}
		if line.TaxMinor > 0 || line.TaxRateBPS > 0 {
			taxID := deterministicChildID("TAX", order.OrderID, line.ID, 0)
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_tax_line (
					tax_line_id, order_id, order_item_id, jurisdiction,
					rate_basis_points, taxable_minor, amount_minor
				) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				taxID, order.OrderID, line.ID, jurisdiction,
				line.TaxRateBPS, line.TotalMinor, line.TaxMinor); err != nil {
				return fmt.Errorf("insert order tax line: %w", err)
			}
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_payment_projection (
			order_id, captured_minor, refunded_minor, currency, updated_at
		) VALUES ($1, 0, 0, $2, $3)`,
		order.OrderID, order.Currency, order.PlacedAt); err != nil {
		return fmt.Errorf("insert order payment projection: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_checkout_detail (order_id, coupon_code)
		VALUES ($1, NULLIF($2, ''))`, order.OrderID, order.CouponCode); err != nil {
		return fmt.Errorf("insert order checkout detail: %w", err)
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

func mustJSON(value any) []byte {
	body, _ := json.Marshal(value)
	return body
}

func defaultText(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func commandFingerprint(value any) string {
	body, _ := json.Marshal(value)
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])
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
		       COALESCE(s.state, ''), COALESCE(d.coupon_code, '')
		FROM sales_order o
		LEFT JOIN order_customer_snapshot c USING (order_id)
		LEFT JOIN order_saga s USING (order_id)
		LEFT JOIN order_checkout_detail d USING (order_id)
		WHERE o.order_id = $1`, id).
		Scan(&order.OrderID, &order.Number, &order.CustomerID, &order.Status,
			&order.PaymentStatus, &order.FulfillmentStatus, &order.Currency,
			&order.SubtotalMinor, &order.DiscountMinor, &order.ShippingMinor,
			&order.TaxMinor, &order.TotalMinor, &address,
			&order.IdempotencyKey, &order.PlacedAt,
			&order.Customer.Email, &order.Customer.Name, &order.Customer.Phone,
			&billingAddress, &order.SagaState, &order.CouponCode)
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
		SELECT i.order_item_id, i.sku, i.title, i.quantity, i.unit_price_minor,
		       i.discount_minor, i.total_minor,
		       COALESCE(s.product_id, ''), COALESCE(s.product_title, ''),
		       COALESCE(s.variant_title, ''), COALESCE(s.attributes, '{}'::jsonb),
		       COALESCE(s.weight_grams, 0), COALESCE(s.channel, ''),
		       COALESCE(t.amount_minor, 0), COALESCE(t.rate_basis_points, 0)
		FROM order_item i
		LEFT JOIN order_item_snapshot s USING (order_item_id)
		LEFT JOIN order_tax_line t ON t.order_item_id = i.order_item_id
		WHERE i.order_id = $1
		ORDER BY i.order_item_id`, id)
	if err != nil {
		return Order{}, fmt.Errorf("query order items: %w", err)
	}
	defer rows.Close()
	order.Lines = []OrderLine{}
	for rows.Next() {
		var line OrderLine
		var attributes []byte
		if err := rows.Scan(&line.ID, &line.SKU, &line.Title, &line.Quantity,
			&line.UnitPriceMinor, &line.DiscountMinor, &line.TotalMinor,
			&line.ProductID, &line.ProductTitle, &line.VariantTitle, &attributes,
			&line.WeightGrams, &line.Channel, &line.TaxMinor, &line.TaxRateBPS); err != nil {
			return Order{}, fmt.Errorf("scan order item: %w", err)
		}
		if err := json.Unmarshal(attributes, &line.Attributes); err != nil {
			return Order{}, fmt.Errorf("decode order item attributes: %w", err)
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
	_ []messaging.Event,
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
	hash := commandFingerprint(struct {
		OrderID string `json:"order_id"`
		Reason  string `json:"reason"`
	}{command.OrderID, command.Reason})
	scope := "cancel:" + command.OrderID
	var priorHash string
	err = tx.QueryRow(ctx, `
		SELECT request_hash FROM order_command
		WHERE command_scope = $1 AND idempotency_key = $2`,
		scope, command.IdempotencyKey).Scan(&priorHash)
	if err == nil {
		if priorHash != hash {
			return Order{}, ErrIdempotencyConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Order{}, fmt.Errorf("commit cancellation replay: %w", err)
		}
		replayed, err := store.GetOrder(ctx, command.OrderID)
		replayed.Replayed = true
		return replayed, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Order{}, fmt.Errorf("query cancellation replay: %w", err)
	}
	if order.Status == "cancelled" || (order.Status != "pending" && order.Status != "confirmed") {
		return Order{}, ErrInvalidCommand
	}
	now := store.clock().UTC()
	update, err := tx.Exec(ctx, `
		UPDATE sales_order SET status = 'cancelled'
		WHERE order_id = $1 AND status = $2`,
		command.OrderID, order.Status)
	if err != nil {
		return Order{}, fmt.Errorf("cancel sales order: %w", err)
	}
	if update.RowsAffected() != 1 {
		return Order{}, ErrVersionConflict
	}
	update, err = tx.Exec(ctx, `
		UPDATE order_saga
		SET state = 'compensating', version = version + 1,
		    updated_at = $2
		WHERE order_id = $1 AND state <> 'failed'`,
		command.OrderID, now)
	if err != nil {
		return Order{}, fmt.Errorf("advance cancellation saga: %w", err)
	}
	if update.RowsAffected() != 1 {
		return Order{}, ErrInvalidCommand
	}
	historyID := deterministicChildID("HIST", command.OrderID, "cancel:"+command.IdempotencyKey, 0)
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_status_history (
			event_id, order_id, from_status, to_status, reason, occurred_at
		) VALUES ($1, $2, $3, 'cancelled', $4, $5)`,
		historyID, command.OrderID, order.Status, command.Reason, now); err != nil {
		return Order{}, fmt.Errorf("insert cancellation history: %w", err)
	}
	cancelled := newOrderEvent("order.cancelled.v1", order, command.CorrelationID, command.CausationID,
		map[string]any{"order_id": order.OrderID, "reason": command.Reason}, now)
	if err := messaging.InsertOutbox(ctx, tx, cancelled); err != nil {
		return Order{}, fmt.Errorf("insert cancellation outbox: %w", err)
	}
	nextType, nextStep := "inventory.release-requested.v1", "inventory_release_requested"
	if order.PaymentStatus == "paid" || order.PaymentStatus == "authorized" ||
		order.PaymentStatus == "partially_refunded" {
		nextType, nextStep = "fulfillment.cancel-requested.v1", "fulfillment_cancel_requested"
	}
	next := newOrderEvent(nextType, order, command.CorrelationID, cancelled.ID,
		map[string]any{"order_id": order.OrderID, "reason": command.Reason}, now)
	if err := messaging.InsertOutbox(ctx, tx, next); err != nil {
		return Order{}, fmt.Errorf("insert cancellation step outbox: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_saga_step (saga_id, step, status, updated_at)
		SELECT saga_id, $2, 'pending', $3
		FROM order_saga WHERE order_id = $1
		ON CONFLICT (saga_id, step) DO NOTHING`,
		command.OrderID, nextStep, now); err != nil {
		return Order{}, fmt.Errorf("create cancellation saga step: %w", err)
	}
	response := mustJSON(map[string]any{"order_id": command.OrderID})
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_command (
			command_scope, idempotency_key, request_hash, resource_id, response, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		scope, command.IdempotencyKey, hash, command.OrderID, response, now); err != nil {
		if uniqueViolation(err) {
			return Order{}, ErrIdempotencyConflict
		}
		return Order{}, fmt.Errorf("insert cancellation replay: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Order{}, fmt.Errorf("commit cancellation: %w", err)
	}
	return store.GetOrder(ctx, command.OrderID)
}

func getOrderForUpdate(ctx context.Context, tx pgx.Tx, id string) (Order, error) {
	var order Order
	err := tx.QueryRow(ctx, `
		SELECT order_id, status, payment_status, fulfillment_status, total_minor, currency
		FROM sales_order WHERE order_id = $1 FOR UPDATE`, id).
		Scan(&order.OrderID, &order.Status, &order.PaymentStatus,
			&order.FulfillmentStatus, &order.TotalMinor, &order.Currency)
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
	case "inventory.committed.v1", "inventory.reservation-committed.v1":
		return store.applyInventoryCommitted(ctx, tx, event)
	case "inventory.released.v1", "inventory.reservation-released.v1":
		return store.applyInventoryReleased(ctx, tx, event)
	case "fulfillment.cancelled.v1", "fulfillment.cancellation-succeeded.v1":
		return store.applyFulfillmentCancelled(ctx, tx, event)
	case "fulfillment.completed.v1", "fulfillment.delivered.v1":
		return store.applyFulfillmentCompleted(ctx, tx, event)
	case "payment.refunded.v1", "refund.succeeded.v1":
		return store.applyRefunded(ctx, tx, event)
	default:
		return messaging.NonRetryable(fmt.Errorf("unsupported order event type %s", event.Type))
	}
}

type orderResultPayload struct {
	OrderID string `json:"order_id"`
}

type paymentFailurePayload struct {
	OrderID string `json:"order_id"`
	Code    string `json:"code"`
}

type inventoryResultPayload struct {
	OrderID     string                  `json:"order_id"`
	Allocations []ReservationAllocation `json:"allocations"`
}

type moneyResultPayload struct {
	OrderID     string `json:"order_id"`
	Currency    string `json:"currency"`
	AmountMinor int64  `json:"amount_minor"`
}

func decodeOrderResult(event messaging.Event, destination any) error {
	if event.SchemaVersion != messaging.CurrentSchemaVersion || event.Subject == "" ||
		event.OccurredAt.IsZero() || len(event.Data) == 0 || !json.Valid(event.Data) {
		return messaging.NonRetryable(fmt.Errorf("invalid %s envelope or payload", event.Type))
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return messaging.NonRetryable(fmt.Errorf("decode %s payload: %w", event.Type, err))
	}
	body, err := json.Marshal(destination)
	if err != nil || string(body) == "{}" {
		return messaging.NonRetryable(fmt.Errorf("empty %s payload", event.Type))
	}
	return nil
}

func validatePayloadOrder(event messaging.Event, orderID string) error {
	if orderID == "" || orderID != event.Subject {
		return messaging.NonRetryable(fmt.Errorf(
			"%s payload order_id %q does not match subject %q",
			event.Type, orderID, event.Subject))
	}
	return nil
}

func (store *PostgresStore) applyPaymentFailed(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	var payload paymentFailurePayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
	if strings.TrimSpace(payload.Code) == "" {
		return messaging.NonRetryable(errors.New("payment failure code is required"))
	}
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "pending" || order.PaymentStatus != "pending" {
		return nil
	}
	now := store.clock().UTC()
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
	if err := updateSagaForPayment(ctx, tx, event, "compensating", "failed", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_saga_step (saga_id, step, status, event_id, updated_at)
		SELECT saga_id, 'inventory_release_requested', 'pending', $2::uuid, $3
		FROM order_saga WHERE order_id = $1
		ON CONFLICT (saga_id, step) DO NOTHING`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("request inventory compensation step: %w", err)
	}
	return insertDerivedEvents(ctx, tx, event, order,
		"inventory.release-requested.v1", "order.cancelled.v1")
}

func (store *PostgresStore) applyPaymentPaid(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	var payload moneyResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "pending" || order.PaymentStatus != "pending" {
		return nil
	}
	if payload.Currency != order.Currency || payload.AmountMinor != order.TotalMinor {
		return messaging.NonRetryable(fmt.Errorf(
			"%s money %s/%d does not match order %s/%d",
			event.Type, payload.Currency, payload.AmountMinor, order.Currency, order.TotalMinor))
	}
	now := store.clock().UTC()
	command, err := tx.Exec(ctx, `
		UPDATE sales_order
		SET payment_status = 'authorized'
		WHERE order_id = $1 AND status = 'pending' AND payment_status = 'pending'`,
		event.Subject)
	if err != nil {
		return fmt.Errorf("apply paid payment: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_payment_projection
		SET captured_minor = $2, updated_at = $3
		WHERE order_id = $1 AND captured_minor = 0`,
		event.Subject, payload.AmountMinor, now); err != nil {
		return fmt.Errorf("record captured payment: %w", err)
	}
	if err := updateSagaForPayment(ctx, tx, event, "reserving", "completed", now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_saga_step (saga_id, step, status, updated_at)
		SELECT saga_id, 'inventory_commit_requested', 'pending', $2
		FROM order_saga WHERE order_id = $1
		ON CONFLICT (saga_id, step) DO NOTHING`,
		event.Subject, now); err != nil {
		return fmt.Errorf("create inventory commit saga step: %w", err)
	}
	return insertDerivedEvents(ctx, tx, event, order, "inventory.commit-requested.v1")
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
		WHERE order_id = $1 AND state IN ('paying', 'reserving')`,
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

func (store *PostgresStore) applyInventoryCommitted(
	ctx context.Context,
	tx pgx.Tx,
	event messaging.Event,
) error {
	var payload inventoryResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
	if err := validateInventoryResult(ctx, tx, event, payload.Allocations, "committed"); err != nil {
		return err
	}
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "pending" || order.PaymentStatus != "authorized" {
		return nil
	}
	now := store.clock().UTC()
	command, err := tx.Exec(ctx, `
		UPDATE sales_order
		SET status = 'confirmed', payment_status = 'paid'
		WHERE order_id = $1 AND status = 'pending' AND payment_status = 'authorized'`,
		event.Subject)
	if err != nil {
		return fmt.Errorf("apply inventory commit: %w", err)
	}
	if command.RowsAffected() != 1 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_inventory_allocation
		SET status = 'committed'
		WHERE order_id = $1 AND status = 'active'`, event.Subject); err != nil {
		return fmt.Errorf("commit inventory allocations: %w", err)
	}
	if err := recordStatusTransition(ctx, tx, event, "pending", "confirmed"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga
		SET state = 'completed', version = version + 1,
		    last_event_id = $2::uuid, updated_at = $3
		WHERE order_id = $1 AND state = 'reserving'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete checkout saga: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'completed', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'inventory_commit_requested' AND status = 'pending'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete inventory commit step: %w", err)
	}
	return insertDerivedEvents(ctx, tx, event, order, "order.paid.v1")
}

func (store *PostgresStore) applyInventoryReleased(
	ctx context.Context,
	tx pgx.Tx,
	event messaging.Event,
) error {
	var payload inventoryResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
	if err := validateInventoryResult(ctx, tx, event, payload.Allocations, "released"); err != nil {
		return err
	}
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "cancelled" {
		return nil
	}
	var pending int
	if err := tx.QueryRow(ctx, `
		SELECT count(*)
		FROM order_saga_step
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step IN ('refund_requested', 'fulfillment_cancel_requested')
		  AND status = 'pending'`, event.Subject).Scan(&pending); err != nil {
		return fmt.Errorf("check compensation prerequisites: %w", err)
	}
	if pending != 0 {
		return messaging.NonRetryable(errors.New("inventory release arrived before compensation prerequisites"))
	}
	now := store.clock().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE order_inventory_allocation SET status = 'released'
		WHERE order_id = $1 AND status IN ('active', 'committed')`, event.Subject); err != nil {
		return fmt.Errorf("release inventory allocations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'completed', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'inventory_release_requested' AND status = 'pending'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete inventory release step: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga
		SET state = 'failed', version = version + 1,
		    last_event_id = $2::uuid, updated_at = $3
		WHERE order_id = $1 AND state = 'compensating'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete compensation saga: %w", err)
	}
	return nil
}

func validateInventoryResult(
	ctx context.Context,
	tx pgx.Tx,
	event messaging.Event,
	allocations []ReservationAllocation,
	expectedStatus string,
) error {
	var stored int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM order_inventory_allocation WHERE order_id = $1`,
		event.Subject).Scan(&stored); err != nil {
		return fmt.Errorf("count inventory allocation snapshot: %w", err)
	}
	if stored == 0 && len(allocations) == 0 {
		return nil
	}
	if len(allocations) != stored {
		return messaging.NonRetryable(fmt.Errorf("%s allocation count mismatch", event.Type))
	}
	seen := make(map[string]bool, len(allocations))
	for _, allocation := range allocations {
		if allocation.AllocationID == "" || allocation.Status != expectedStatus ||
			seen[allocation.AllocationID] {
			return messaging.NonRetryable(fmt.Errorf("%s invalid allocation result", event.Type))
		}
		seen[allocation.AllocationID] = true
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			  SELECT 1 FROM order_inventory_allocation
			  WHERE order_id = $1 AND allocation_id = $2 AND sku = $3 AND quantity = $4
			)`, event.Subject, allocation.AllocationID, allocation.SKU, allocation.Quantity).Scan(&exists); err != nil {
			return fmt.Errorf("validate inventory allocation result: %w", err)
		}
		if !exists {
			return messaging.NonRetryable(fmt.Errorf("%s allocation identity mismatch", event.Type))
		}
	}
	return nil
}

func (store *PostgresStore) applyFulfillmentCancelled(
	ctx context.Context,
	tx pgx.Tx,
	event messaging.Event,
) error {
	var payload orderResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "cancelled" {
		return nil
	}
	now := store.clock().UTC()
	command, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'completed', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'fulfillment_cancel_requested' AND status = 'pending'`,
		event.Subject, event.ID, now)
	if err != nil {
		return fmt.Errorf("complete fulfillment cancellation: %w", err)
	}
	if command.RowsAffected() == 0 {
		return nil
	}
	var captured, refunded int64
	if err := tx.QueryRow(ctx, `
		SELECT captured_minor, refunded_minor
		FROM order_payment_projection WHERE order_id = $1`,
		event.Subject).Scan(&captured, &refunded); err != nil {
		return fmt.Errorf("read refund projection: %w", err)
	}
	if captured > refunded {
		if err := upsertPendingSagaStep(ctx, tx, event.Subject, "refund_requested", now); err != nil {
			return err
		}
		refund := newOrderEvent("payment.refund-requested.v1", order,
			event.CorrelationID, event.ID, map[string]any{
				"order_id": order.OrderID, "currency": order.Currency,
				"amount_minor": captured - refunded,
			}, now)
		return messaging.InsertOutbox(ctx, tx, refund)
	}
	return store.requestInventoryRelease(ctx, tx, event, order)
}

func (store *PostgresStore) applyFulfillmentCompleted(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	var payload orderResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
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
		event.Subject, event.ID, store.clock().UTC()); err != nil {
		return fmt.Errorf("complete fulfillment saga step: %w", err)
	}
	return nil
}

func (store *PostgresStore) applyRefunded(ctx context.Context, tx pgx.Tx, event messaging.Event) error {
	var payload moneyResultPayload
	if err := decodeOrderResult(event, &payload); err != nil {
		return err
	}
	if err := validatePayloadOrder(event, payload.OrderID); err != nil {
		return err
	}
	order, err := getOrderForUpdate(ctx, tx, event.Subject)
	if err != nil {
		return err
	}
	if order.Status != "cancelled" {
		return nil
	}
	if payload.Currency != order.Currency || payload.AmountMinor <= 0 {
		return messaging.NonRetryable(fmt.Errorf("invalid refund money"))
	}
	var captured, refunded int64
	if err := tx.QueryRow(ctx, `
		SELECT captured_minor, refunded_minor
		FROM order_payment_projection WHERE order_id = $1 FOR UPDATE`,
		event.Subject).Scan(&captured, &refunded); err != nil {
		return fmt.Errorf("lock refund projection: %w", err)
	}
	if payload.AmountMinor > captured-refunded {
		return messaging.NonRetryable(fmt.Errorf("refund exceeds remaining captured amount"))
	}
	refunded += payload.AmountMinor
	now := store.clock().UTC()
	if _, err := tx.Exec(ctx, `
		UPDATE order_payment_projection
		SET refunded_minor = $2, updated_at = $3 WHERE order_id = $1`,
		event.Subject, refunded, now); err != nil {
		return fmt.Errorf("accumulate refund: %w", err)
	}
	status := "partially_refunded"
	if refunded == captured {
		status = "refunded"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sales_order SET payment_status = $2
		WHERE order_id = $1 AND status = 'cancelled'`,
		event.Subject, status); err != nil {
		return fmt.Errorf("apply refund status: %w", err)
	}
	if refunded < captured {
		refund := newOrderEvent("payment.refund-requested.v1", order,
			event.CorrelationID, event.ID, map[string]any{
				"order_id": order.OrderID, "currency": order.Currency,
				"amount_minor": captured - refunded,
			}, now)
		return messaging.InsertOutbox(ctx, tx, refund)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE order_saga_step
		SET status = 'completed', event_id = $2::uuid, updated_at = $3
		WHERE saga_id = (SELECT saga_id FROM order_saga WHERE order_id = $1)
		  AND step = 'refund_requested' AND status = 'pending'`,
		event.Subject, event.ID, now); err != nil {
		return fmt.Errorf("complete refund saga step: %w", err)
	}
	return store.requestInventoryRelease(ctx, tx, event, order)
}

func upsertPendingSagaStep(
	ctx context.Context,
	tx pgx.Tx,
	orderID string,
	step string,
	now time.Time,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO order_saga_step (saga_id, step, status, updated_at)
		SELECT saga_id, $2, 'pending', $3 FROM order_saga WHERE order_id = $1
		ON CONFLICT (saga_id, step) DO UPDATE
		SET status = CASE
		  WHEN order_saga_step.status = 'completed' THEN order_saga_step.status
		  ELSE 'pending'
		END, updated_at = EXCLUDED.updated_at`,
		orderID, step, now); err != nil {
		return fmt.Errorf("upsert saga step %s: %w", step, err)
	}
	return nil
}

func (store *PostgresStore) requestInventoryRelease(
	ctx context.Context,
	tx pgx.Tx,
	cause messaging.Event,
	order Order,
) error {
	if err := upsertPendingSagaStep(ctx, tx, order.OrderID, "inventory_release_requested", store.clock().UTC()); err != nil {
		return err
	}
	release := newOrderEvent("inventory.release-requested.v1", order,
		cause.CorrelationID, cause.ID, map[string]any{"order_id": order.OrderID}, cause.OccurredAt.UTC())
	return messaging.InsertOutbox(ctx, tx, release)
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
