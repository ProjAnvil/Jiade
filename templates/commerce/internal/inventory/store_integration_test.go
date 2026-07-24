//go:build integration

package inventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresReserveIsAtomicIdempotentAndPriorityOrdered(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-1", []integrationLevel{
		{location: "LOC-Z", priority: 1, onHand: 1},
		{location: "LOC-A", priority: 2, onHand: 5},
	})
	now := fixedInventoryClock()
	command := ReserveCommand{
		OrderID: "ORD-1", IdempotencyKey: "reserve-1", CorrelationID: "request-1",
		Lines:      []ReserveLine{{SKU: "SKU-1", Quantity: 4}},
		OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
	}
	first, err := store.Reserve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Reserve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Existing || !second.Existing || len(first.Allocations) != 2 ||
		first.Allocations[0].LocationID != "LOC-Z" || first.Allocations[0].Quantity != 1 ||
		first.Allocations[1].LocationID != "LOC-A" || first.Allocations[1].Quantity != 3 ||
		fmt.Sprint(first.Allocations) != fmt.Sprint(second.Allocations) {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	assertIntegrationCount(t, pool, "reservation", 2)
	assertIntegrationCount(t, pool, "stock_movement", 2)
	assertIntegrationCount(t, pool, "outbox_event", 1)

	command.Lines[0].Quantity = 3
	if _, err := store.Reserve(context.Background(), command); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different payload error=%v", err)
	}
	assertIntegrationCount(t, pool, "reservation", 2)
	assertIntegrationCount(t, pool, "stock_movement", 2)
	assertIntegrationCount(t, pool, "outbox_event", 1)
}

func TestPostgresConcurrentReservationsNeverOversell(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-RACE", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	now := fixedInventoryClock()
	var successes atomic.Int32
	var insufficient atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 10; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.Reserve(context.Background(), ReserveCommand{
				OrderID: fmt.Sprintf("ORD-%d", index), IdempotencyKey: fmt.Sprintf("reserve-%d", index),
				CorrelationID: fmt.Sprintf("request-%d", index),
				Lines:         []ReserveLine{{SKU: "SKU-RACE", Quantity: 1}},
				OccurredAt:    now, ExpiresAt: now.Add(DefaultReservationTTL),
			})
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrInsufficientStock):
				insufficient.Add(1)
			default:
				t.Errorf("reservation %d: %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	if successes.Load() != 5 || insufficient.Load() != 5 {
		t.Fatalf("successes=%d insufficient=%d", successes.Load(), insufficient.Load())
	}
	var onHand, reserved int64
	if err := pool.QueryRow(context.Background(), `
		SELECT on_hand, reserved FROM inventory_level
		WHERE sku = 'SKU-RACE' AND location_id = 'LOC-1'`).Scan(&onHand, &reserved); err != nil {
		t.Fatal(err)
	}
	if onHand != 5 || reserved != 5 {
		t.Fatalf("on_hand=%d reserved=%d", onHand, reserved)
	}
}

func TestPostgresConcurrentSameKeyCreatesOneReservation(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-SAME", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	now := fixedInventoryClock()
	command := ReserveCommand{
		OrderID: "ORD-SAME", IdempotencyKey: "same-key", CorrelationID: "same-request",
		Lines:      []ReserveLine{{SKU: "SKU-SAME", Quantity: 2}},
		OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
	}
	results := make(chan ReservationResult, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := store.Reserve(context.Background(), command)
			results <- result
			errs <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	existing := 0
	for result := range results {
		if result.Existing {
			existing++
		}
	}
	if existing != 1 {
		t.Fatalf("existing results=%d, want one replay", existing)
	}
	assertIntegrationCount(t, pool, "reservation", 1)
	assertIntegrationCount(t, pool, "stock_movement", 1)
	assertIntegrationCount(t, pool, "outbox_event", 1)
}

func TestPostgresTerminalBeforeReserveFencesOrder(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-FENCE", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	now := fixedInventoryClock()
	if allocations, changed, err := store.TransitionOrder(context.Background(), "ORD-FENCE", ReservationRelease, now); err != nil || !changed || len(allocations) != 0 {
		t.Fatalf("terminal before reserve allocations=%v changed=%v err=%v", allocations, changed, err)
	}
	if _, changed, err := store.TransitionOrder(context.Background(), "ORD-FENCE", ReservationRelease, now); err != nil || changed {
		t.Fatalf("terminal replay changed=%v err=%v", changed, err)
	}
	if _, _, err := store.TransitionOrder(context.Background(), "ORD-FENCE", ReservationCommit, now); !errors.Is(err, ErrOrderTerminal) {
		t.Fatalf("different terminal error=%v", err)
	}
	_, err := store.Reserve(context.Background(), ReserveCommand{
		OrderID: "ORD-FENCE", IdempotencyKey: "late-key", CorrelationID: "late-request",
		Lines:      []ReserveLine{{SKU: "SKU-FENCE", Quantity: 1}},
		OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
	})
	if !errors.Is(err, ErrOrderTerminal) {
		t.Fatalf("late reserve error=%v", err)
	}
	assertIntegrationCount(t, pool, "reservation_order_state", 1)
	assertIntegrationCount(t, pool, "reservation", 0)
	assertIntegrationCount(t, pool, "stock_movement", 0)
	assertIntegrationCount(t, pool, "outbox_event", 1)
}

func TestPostgresConcurrentReserveAndTerminalLeaveNoActiveStock(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-FENCE-RACE", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	now := fixedInventoryClock()
	start := make(chan struct{})
	var reserveErr, terminalErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, reserveErr = store.Reserve(context.Background(), ReserveCommand{
			OrderID: "ORD-FENCE-RACE", IdempotencyKey: "race-key", CorrelationID: "race-request",
			Lines:      []ReserveLine{{SKU: "SKU-FENCE-RACE", Quantity: 2}},
			OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
		})
	}()
	go func() {
		defer wait.Done()
		<-start
		_, _, terminalErr = store.TransitionOrder(context.Background(), "ORD-FENCE-RACE", ReservationRelease, now)
	}()
	close(start)
	wait.Wait()
	if terminalErr != nil {
		t.Fatalf("terminal error=%v", terminalErr)
	}
	if reserveErr != nil && !errors.Is(reserveErr, ErrOrderTerminal) {
		t.Fatalf("reserve error=%v", reserveErr)
	}
	var reserved, active int64
	if err := pool.QueryRow(context.Background(), `
		SELECT reserved FROM inventory_level
		WHERE sku = 'SKU-FENCE-RACE' AND location_id = 'LOC-1'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*) FROM reservation
		WHERE order_id = 'ORD-FENCE-RACE' AND status = 'active'`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || active != 0 {
		t.Fatalf("reserved=%d active=%d", reserved, active)
	}
	assertIntegrationCount(t, pool, "reservation_order_state", 1)
}

func TestPostgresRequiresEnabledLocationProfile(t *testing.T) {
	tests := []struct {
		name          string
		profileExists bool
		enabled       bool
	}{
		{name: "missing profile"},
		{name: "disabled profile", profileExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, pool := newIntegrationInventoryStore(t)
			seedIntegrationLevelWithProfile(t, pool, "SKU-PROFILE", "LOC-1", 1, 5, test.profileExists, test.enabled)
			now := fixedInventoryClock()
			_, err := store.Reserve(context.Background(), ReserveCommand{
				OrderID: "ORD-PROFILE", IdempotencyKey: "profile-key", CorrelationID: "profile-request",
				Lines:      []ReserveLine{{SKU: "SKU-PROFILE", Quantity: 1}},
				OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
			})
			if !errors.Is(err, ErrInsufficientStock) {
				t.Fatalf("Reserve() error=%v, want ErrInsufficientStock", err)
			}
		})
	}
}

func TestPostgresMultiSKULockOrderAvoidsDeadlock(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-A", []integrationLevel{{location: "LOC-1", priority: 1, onHand: 10}})
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO inventory_level (sku, location_id, on_hand, reserved, updated_at)
		VALUES ('SKU-B', 'LOC-1', 10, 0, $1)`, fixedInventoryClock()); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, fixedInventoryClock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var successes atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			lines := []ReserveLine{{SKU: "SKU-A", Quantity: 1}, {SKU: "SKU-B", Quantity: 1}}
			if index%2 == 1 {
				lines[0], lines[1] = lines[1], lines[0]
			}
			_, err := service.Reserve(ctx, ReserveCommand{
				OrderID:        fmt.Sprintf("ORD-MULTI-%d", index),
				IdempotencyKey: fmt.Sprintf("multi-%d", index),
				Lines:          lines,
			})
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrInsufficientStock) {
				t.Errorf("reserve %d: %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		t.Fatalf("multi-SKU reservations timed out: %v", err)
	}
	if successes.Load() != 10 {
		t.Fatalf("successes=%d, want 10", successes.Load())
	}
}

func TestPostgresDuplicateTerminalOperationsAreNoOps(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-TERM", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	now := fixedInventoryClock()
	_, err := store.Reserve(context.Background(), ReserveCommand{
		OrderID: "ORD-TERM", IdempotencyKey: "term-key", CorrelationID: "term-request",
		Lines:      []ReserveLine{{SKU: "SKU-TERM", Quantity: 2}},
		OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.TransitionOrder(context.Background(), "ORD-TERM", ReservationRelease, now); err != nil || !changed {
		t.Fatalf("first release changed=%v err=%v", changed, err)
	}
	if _, changed, err := store.TransitionOrder(context.Background(), "ORD-TERM", ReservationRelease, now); err != nil || changed {
		t.Fatalf("duplicate release changed=%v err=%v", changed, err)
	}
	if _, changed, err := store.TransitionOrder(context.Background(), "ORD-TERM", ReservationCommit, now); !errors.Is(err, ErrOrderTerminal) || changed {
		t.Fatalf("late commit changed=%v err=%v", changed, err)
	}
	assertIntegrationCount(t, pool, "stock_movement", 2)
	assertIntegrationCount(t, pool, "outbox_event", 2)
	var reserved int64
	if err := pool.QueryRow(context.Background(), `
		SELECT reserved FROM inventory_level
		WHERE sku = 'SKU-TERM' AND location_id = 'LOC-1'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatalf("reserved=%d", reserved)
	}
}

func TestPostgresCommitReclassifiesHoldAsSaleWithoutChangingAvailability(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-COMMIT", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	now := fixedInventoryClock()
	_, err := store.Reserve(context.Background(), ReserveCommand{
		OrderID: "ORD-COMMIT", IdempotencyKey: "commit-key", CorrelationID: "commit-request",
		Lines:      []ReserveLine{{SKU: "SKU-COMMIT", Quantity: 2}},
		OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
	})
	if err != nil {
		t.Fatal(err)
	}
	var availableBefore int64
	if err := pool.QueryRow(context.Background(), `
		SELECT available FROM inventory_level
		WHERE sku = 'SKU-COMMIT' AND location_id = 'LOC-1'`).Scan(&availableBefore); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := store.TransitionOrder(context.Background(), "ORD-COMMIT", ReservationCommit, now); err != nil || !changed {
		t.Fatalf("commit changed=%v err=%v", changed, err)
	}
	if _, changed, err := store.TransitionOrder(context.Background(), "ORD-COMMIT", ReservationCommit, now); err != nil || changed {
		t.Fatalf("duplicate commit changed=%v err=%v", changed, err)
	}
	var onHand, reserved, availableAfter, movementDelta int64
	if err := pool.QueryRow(context.Background(), `
		SELECT on_hand, reserved, available FROM inventory_level
		WHERE sku = 'SKU-COMMIT' AND location_id = 'LOC-1'`).
		Scan(&onHand, &reserved, &availableAfter); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(sum(delta), 0) FROM stock_movement
		WHERE sku = 'SKU-COMMIT' AND location_id = 'LOC-1'`).Scan(&movementDelta); err != nil {
		t.Fatal(err)
	}
	if onHand != 3 || reserved != 0 || availableBefore != 3 || availableAfter != 3 ||
		movementDelta != -2 {
		t.Fatalf("on_hand=%d reserved=%d before=%d after=%d movement_delta=%d",
			onHand, reserved, availableBefore, availableAfter, movementDelta)
	}
	assertIntegrationCount(t, pool, "stock_movement", 3)
	assertIntegrationCount(t, pool, "outbox_event", 2)
}

func TestPostgresOutboxFailureRollsBackReservationMovementAndLevel(t *testing.T) {
	store, pool := newIntegrationInventoryStore(t)
	seedIntegrationLevels(t, pool, "SKU-ROLLBACK", []integrationLevel{
		{location: "LOC-1", priority: 1, onHand: 5},
	})
	if _, err := pool.Exec(context.Background(), `DROP TABLE outbox_event`); err != nil {
		t.Fatal(err)
	}
	now := fixedInventoryClock()
	_, err := store.Reserve(context.Background(), ReserveCommand{
		OrderID: "ORD-ROLLBACK", IdempotencyKey: "rollback-key", CorrelationID: "rollback-request",
		Lines:      []ReserveLine{{SKU: "SKU-ROLLBACK", Quantity: 2}},
		OccurredAt: now, ExpiresAt: now.Add(DefaultReservationTTL),
	})
	if err == nil {
		t.Fatal("reservation unexpectedly succeeded without outbox table")
	}
	assertIntegrationCount(t, pool, "reservation", 0)
	assertIntegrationCount(t, pool, "stock_movement", 0)
	var reserved int64
	if err := pool.QueryRow(context.Background(), `
		SELECT reserved FROM inventory_level
		WHERE sku = 'SKU-ROLLBACK' AND location_id = 'LOC-1'`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 {
		t.Fatalf("reserved=%d after rollback", reserved)
	}
}

type integrationLevel struct {
	location string
	priority int
	onHand   int64
}

func newIntegrationInventoryStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("inventory integration test skipped: TEST_DATABASE_URL for a dedicated PostgreSQL database is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open dedicated TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(admin.Close)
	if err := admin.Ping(ctx); err != nil {
		t.Fatalf("dedicated PostgreSQL at TEST_DATABASE_URL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("task5_inventory_%d", time.Now().UnixNano())
	schema = regexp.MustCompile(`\W`).ReplaceAllString(schema, "_")
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop integration schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migration, err := os.ReadFile("../../db/migrations/inventory_db.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply inventory migration: %v", err)
	}
	return NewPostgresStore(pool), pool
}

func seedIntegrationLevels(t *testing.T, pool *pgxpool.Pool, sku string, levels []integrationLevel) {
	t.Helper()
	for _, level := range levels {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO location (location_id, name, type, priority)
			VALUES ($1, $2, 'warehouse', $3)`,
			level.location, level.location, level.priority); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO location_profile (location_id, region, fulfills_orders, time_zone)
			VALUES ($1, 'test', true, 'UTC')`, level.location); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO inventory_level (sku, location_id, on_hand, reserved, updated_at)
			VALUES ($1, $2, $3, 0, $4)`,
			sku, level.location, level.onHand, fixedInventoryClock()); err != nil {
			t.Fatal(err)
		}
	}
}

func seedIntegrationLevelWithProfile(
	t *testing.T,
	pool *pgxpool.Pool,
	sku, location string,
	priority int,
	onHand int64,
	profileExists, enabled bool,
) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO location (location_id, name, type, priority)
		VALUES ($1, $1, 'warehouse', $2)`, location, priority); err != nil {
		t.Fatal(err)
	}
	if profileExists {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO location_profile (location_id, region, fulfills_orders, time_zone)
			VALUES ($1, 'test', $2, 'UTC')`, location, enabled); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO inventory_level (sku, location_id, on_hand, reserved, updated_at)
		VALUES ($1, $2, $3, 0, $4)`, sku, location, onHand, fixedInventoryClock()); err != nil {
		t.Fatal(err)
	}
}

func assertIntegrationCount(t *testing.T, pool *pgxpool.Pool, table string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s count=%d want=%d", table, got, want)
	}
}
