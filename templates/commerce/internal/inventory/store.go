package inventory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"commerce/internal/platform/messaging"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct{ pool *pgxpool.Pool }

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (store *PostgresStore) ListLevels(ctx context.Context, after InventoryCursor, limit int) ([]InventoryLevel, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT i.sku, i.location_id, l.name, l.priority, i.on_hand,
		       i.reserved, i.available, i.updated_at
		FROM inventory_level i
		JOIN location l USING (location_id)
		WHERE (i.sku, i.location_id) > ($1, $2)
		ORDER BY i.sku, i.location_id
		LIMIT $3`, after.SKU, after.LocationID, limit)
	if err != nil {
		return nil, fmt.Errorf("query inventory levels: %w", err)
	}
	defer rows.Close()
	return scanLevels(rows, limit)
}

func (store *PostgresStore) GetLevelsBySKU(ctx context.Context, sku string) ([]InventoryLevel, error) {
	rows, err := store.pool.Query(ctx, `
		SELECT i.sku, i.location_id, l.name, l.priority, i.on_hand,
		       i.reserved, i.available, i.updated_at
		FROM inventory_level i
		JOIN location l USING (location_id)
		WHERE i.sku = $1
		ORDER BY l.priority, i.location_id`, sku)
	if err != nil {
		return nil, fmt.Errorf("query SKU inventory: %w", err)
	}
	defer rows.Close()
	levels, err := scanLevels(rows, 4)
	if err != nil {
		return nil, err
	}
	if len(levels) == 0 {
		return nil, ErrSKUNotFound
	}
	return levels, nil
}

type levelRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

func scanLevels(rows levelRows, capacity int) ([]InventoryLevel, error) {
	levels := make([]InventoryLevel, 0, capacity)
	for rows.Next() {
		var level InventoryLevel
		if err := rows.Scan(&level.SKU, &level.LocationID, &level.LocationName, &level.Priority,
			&level.OnHand, &level.Reserved, &level.Available, &level.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan inventory level: %w", err)
		}
		levels = append(levels, level)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate inventory levels: %w", err)
	}
	return levels, nil
}

func (store *PostgresStore) ListReservationsByOrder(ctx context.Context, orderID string) ([]ReservationAllocation, error) {
	return queryAllocations(ctx, store.pool, `
		SELECT r.reservation_id, r.order_id, r.sku, r.location_id,
		       r.quantity, r.status, r.expires_at
		FROM reservation r
		JOIN location l USING (location_id)
		WHERE r.order_id = $1
		ORDER BY r.sku, l.priority, r.location_id, r.reservation_id`, orderID)
}

func (store *PostgresStore) Reserve(ctx context.Context, command ReserveCommand) (ReservationResult, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ReservationResult{}, fmt.Errorf("begin inventory reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, command.IdempotencyKey); err != nil {
		return ReservationResult{}, fmt.Errorf("lock reservation idempotency key: %w", err)
	}
	keyPrefix := reservationKeyPrefix(command.IdempotencyKey)
	existing, err := existingAllocations(ctx, tx, keyPrefix)
	if err != nil {
		return ReservationResult{}, err
	}
	if len(existing) > 0 {
		if !allocationsMatchCommand(existing, command) {
			return ReservationResult{}, ErrIdempotencyConflict
		}
		return ReservationResult{OrderID: command.OrderID, Allocations: existing, Existing: true}, nil
	}

	allocations := make([]ReservationAllocation, 0, len(command.Lines))
	allocationIndex := 0
	for _, line := range command.Lines {
		levels, err := lockCandidateLevels(ctx, tx, line.SKU)
		if err != nil {
			return ReservationResult{}, err
		}
		if len(levels) == 0 {
			return ReservationResult{}, ErrSKUNotFound
		}
		remaining := line.Quantity
		for _, candidate := range levels {
			if !candidate.Fulfills {
				continue
			}
			available, err := (Level{OnHand: candidate.OnHand, Reserved: candidate.Reserved}).Available()
			if err != nil {
				return ReservationResult{}, err
			}
			if available == 0 {
				continue
			}
			quantity := remaining
			if quantity > available {
				quantity = available
			}
			allocationIndex++
			allocation, err := reserveAtLevel(ctx, tx, command, line.SKU, candidate, quantity, keyPrefix, allocationIndex)
			if err != nil {
				return ReservationResult{}, err
			}
			allocations = append(allocations, allocation)
			remaining -= quantity
			if remaining == 0 {
				break
			}
		}
		if remaining != 0 {
			return ReservationResult{}, ErrInsufficientStock
		}
	}
	if err := insertInventoryEvent(ctx, tx, "inventory.reserved.v1", command.OrderID,
		command.CorrelationID, allocations, command.OccurredAt); err != nil {
		return ReservationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ReservationResult{}, fmt.Errorf("commit inventory reservation: %w", err)
	}
	return ReservationResult{OrderID: command.OrderID, Allocations: allocations}, nil
}

type candidateLevel struct {
	LocationID string
	Priority   int
	OnHand     int64
	Reserved   int64
	Fulfills   bool
}

func lockCandidateLevels(ctx context.Context, tx pgx.Tx, sku string) ([]candidateLevel, error) {
	rows, err := tx.Query(ctx, `
		SELECT i.location_id, l.priority, i.on_hand, i.reserved,
		       COALESCE(p.fulfills_orders, true)
		FROM inventory_level i
		JOIN location l USING (location_id)
		LEFT JOIN location_profile p USING (location_id)
		WHERE i.sku = $1
		ORDER BY l.priority, i.location_id
		FOR UPDATE OF i`, sku)
	if err != nil {
		return nil, fmt.Errorf("lock candidate inventory levels: %w", err)
	}
	defer rows.Close()
	var levels []candidateLevel
	for rows.Next() {
		var level candidateLevel
		if err := rows.Scan(&level.LocationID, &level.Priority, &level.OnHand, &level.Reserved, &level.Fulfills); err != nil {
			return nil, fmt.Errorf("scan candidate inventory level: %w", err)
		}
		levels = append(levels, level)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidate inventory levels: %w", err)
	}
	return levels, nil
}

func reserveAtLevel(
	ctx context.Context,
	tx pgx.Tx,
	command ReserveCommand,
	sku string,
	candidate candidateLevel,
	quantity int64,
	keyPrefix string,
	index int,
) (ReservationAllocation, error) {
	id := deterministicID("RES", command.IdempotencyKey, command.OrderID, sku, candidate.LocationID, fmt.Sprint(index))
	reservation, err := NewReservation(id, command.OrderID, sku, candidate.LocationID,
		fmt.Sprintf("%s:%03d", keyPrefix, index), quantity, command.ExpiresAt)
	if err != nil {
		return ReservationAllocation{}, err
	}
	nextLevel, _, err := (Level{OnHand: candidate.OnHand, Reserved: candidate.Reserved}).Reserve(reservation)
	if err != nil {
		return ReservationAllocation{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE inventory_level
		SET reserved = $1, updated_at = $2
		WHERE sku = $3 AND location_id = $4`,
		nextLevel.Reserved, command.OccurredAt, sku, candidate.LocationID); err != nil {
		return ReservationAllocation{}, fmt.Errorf("update reserved inventory: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO reservation (
			reservation_id, order_id, sku, location_id, quantity,
			status, expires_at, idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		reservation.ID, reservation.OrderID, reservation.SKU, reservation.LocationID,
		reservation.Quantity, reservation.State, reservation.ExpiresAt, reservation.IdempotencyKey); err != nil {
		if isUniqueViolation(err) {
			return ReservationAllocation{}, ErrIdempotencyConflict
		}
		return ReservationAllocation{}, fmt.Errorf("insert reservation allocation: %w", err)
	}
	if err := insertMovement(ctx, tx, reservation, -quantity, "adjustment", "reserve", command.OccurredAt); err != nil {
		return ReservationAllocation{}, err
	}
	return allocationFromReservation(reservation), nil
}

func (store *PostgresStore) TransitionOrder(
	ctx context.Context,
	orderID string,
	event ReservationEvent,
	now time.Time,
) ([]ReservationAllocation, bool, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("begin inventory transition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	allocations, err := queryAllocations(ctx, tx, `
		SELECT r.reservation_id, r.order_id, r.sku, r.location_id,
		       r.quantity, r.status, r.expires_at
		FROM reservation r
		JOIN location l USING (location_id)
		WHERE r.order_id = $1
		ORDER BY r.sku, l.priority, r.location_id, r.reservation_id
		FOR UPDATE OF r`, orderID)
	if err != nil {
		return nil, false, err
	}
	if len(allocations) == 0 {
		return nil, false, ErrReservationNotFound
	}
	changed := false
	for index := range allocations {
		allocation := &allocations[index]
		if allocation.State != ReservationActive ||
			(event == ReservationExpire && allocation.ExpiresAt.After(now)) {
			continue
		}
		var level Level
		if err := tx.QueryRow(ctx, `
			SELECT on_hand, reserved
			FROM inventory_level
			WHERE sku = $1 AND location_id = $2
			FOR UPDATE`, allocation.SKU, allocation.LocationID).
			Scan(&level.OnHand, &level.Reserved); err != nil {
			return nil, false, fmt.Errorf("lock reservation inventory level: %w", err)
		}
		reservation := Reservation{
			ID: allocation.ID, OrderID: allocation.OrderID, SKU: allocation.SKU,
			LocationID: allocation.LocationID, Quantity: allocation.Quantity,
			State: allocation.State, ExpiresAt: allocation.ExpiresAt,
		}
		nextLevel, nextReservation, err := applyReservationTransition(level, reservation, event)
		if err != nil {
			return nil, false, err
		}
		command, err := tx.Exec(ctx, `
			UPDATE reservation SET status = $1
			WHERE reservation_id = $2 AND status = 'active'`,
			nextReservation.State, nextReservation.ID)
		if err != nil {
			return nil, false, fmt.Errorf("update reservation state: %w", err)
		}
		if command.RowsAffected() != 1 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			UPDATE inventory_level
			SET on_hand = $1, reserved = $2, updated_at = $3
			WHERE sku = $4 AND location_id = $5`,
			nextLevel.OnHand, nextLevel.Reserved, now, allocation.SKU, allocation.LocationID); err != nil {
			return nil, false, fmt.Errorf("update transitioned inventory: %w", err)
		}
		if event == ReservationCommit {
			if err := insertMovement(ctx, tx, nextReservation, allocation.Quantity,
				"adjustment", "commit_release", now); err != nil {
				return nil, false, err
			}
			if err := insertMovement(ctx, tx, nextReservation, -allocation.Quantity,
				"sale", "commit", now); err != nil {
				return nil, false, err
			}
		} else {
			if err := insertMovement(ctx, tx, nextReservation, allocation.Quantity,
				"adjustment", string(event), now); err != nil {
				return nil, false, err
			}
		}
		allocation.State = nextReservation.State
		changed = true
	}
	if changed {
		if err := insertInventoryEvent(ctx, tx, inventoryEventType(event), orderID, orderID, allocations, now); err != nil {
			return nil, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit inventory transition: %w", err)
	}
	return allocations, changed, nil
}

func applyReservationTransition(level Level, reservation Reservation, event ReservationEvent) (Level, Reservation, error) {
	switch event {
	case ReservationRelease:
		return level.Release(reservation)
	case ReservationCommit:
		return level.Commit(reservation)
	case ReservationExpire:
		return level.Expire(reservation)
	default:
		return level, reservation, ErrInvalidCommand
	}
}

func insertMovement(
	ctx context.Context,
	tx pgx.Tx,
	reservation Reservation,
	delta int64,
	reason string,
	action string,
	createdAt time.Time,
) error {
	movementID := deterministicID("MOV", reservation.ID, action)
	referenceID := reservation.ID + ":" + action
	_, err := tx.Exec(ctx, `
		INSERT INTO stock_movement (
			movement_id, sku, location_id, delta, reason, reference_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		movementID, reservation.SKU, reservation.LocationID, delta, reason, referenceID, createdAt)
	if err != nil {
		return fmt.Errorf("insert inventory movement: %w", err)
	}
	return nil
}

func insertInventoryEvent(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	orderID string,
	correlationID string,
	allocations []ReservationAllocation,
	occurredAt time.Time,
) error {
	payload, err := json.Marshal(ReservationResult{OrderID: orderID, Allocations: allocations})
	if err != nil {
		return fmt.Errorf("encode inventory event: %w", err)
	}
	event := messaging.NewEvent(eventType, orderID, correlationID, "", payload, func() time.Time { return occurredAt })
	if err := messaging.InsertOutbox(ctx, tx, event); err != nil {
		return fmt.Errorf("insert inventory outbox event: %w", err)
	}
	return nil
}

type allocationQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func queryAllocations(ctx context.Context, queryer allocationQueryer, statement string, argument any) ([]ReservationAllocation, error) {
	rows, err := queryer.Query(ctx, statement, argument)
	if err != nil {
		return nil, fmt.Errorf("query reservation allocations: %w", err)
	}
	defer rows.Close()
	allocations := []ReservationAllocation{}
	for rows.Next() {
		var allocation ReservationAllocation
		if err := rows.Scan(&allocation.ID, &allocation.OrderID, &allocation.SKU,
			&allocation.LocationID, &allocation.Quantity, &allocation.State,
			&allocation.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan reservation allocation: %w", err)
		}
		allocations = append(allocations, allocation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reservation allocations: %w", err)
	}
	return allocations, nil
}

func existingAllocations(ctx context.Context, tx pgx.Tx, prefix string) ([]ReservationAllocation, error) {
	return queryAllocations(ctx, tx, `
		SELECT r.reservation_id, r.order_id, r.sku, r.location_id,
		       r.quantity, r.status, r.expires_at
		FROM reservation r
		JOIN location l USING (location_id)
		WHERE left(r.idempotency_key, length($1) + 1) = $1 || ':'
		ORDER BY r.sku, l.priority, r.location_id, r.reservation_id
		FOR UPDATE OF r`, prefix)
}

func allocationsMatchCommand(allocations []ReservationAllocation, command ReserveCommand) bool {
	if len(allocations) == 0 {
		return false
	}
	totals := make(map[string]int64)
	for _, allocation := range allocations {
		if allocation.OrderID != command.OrderID {
			return false
		}
		totals[allocation.SKU] += allocation.Quantity
	}
	if len(totals) != len(command.Lines) {
		return false
	}
	for _, line := range command.Lines {
		if totals[line.SKU] != line.Quantity {
			return false
		}
	}
	return true
}

func allocationFromReservation(reservation Reservation) ReservationAllocation {
	return ReservationAllocation{
		ID: reservation.ID, OrderID: reservation.OrderID, SKU: reservation.SKU,
		LocationID: reservation.LocationID, Quantity: reservation.Quantity,
		State: reservation.State, ExpiresAt: reservation.ExpiresAt,
	}
}

func reservationKeyPrefix(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "r1_" + hex.EncodeToString(digest[:16])
}

func deterministicID(prefix string, parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return prefix + "-" + hex.EncodeToString(hash.Sum(nil)[:12])
}

func inventoryEventType(event ReservationEvent) string {
	switch event {
	case ReservationRelease:
		return "inventory.released.v1"
	case ReservationCommit:
		return "inventory.committed.v1"
	case ReservationExpire:
		return "inventory.expired.v1"
	default:
		panic("invalid inventory event")
	}
}

func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

var _ Store = (*PostgresStore)(nil)
