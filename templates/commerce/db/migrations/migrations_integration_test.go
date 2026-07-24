//go:build integration

package migrations

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrationsApplyTwiceToDedicatedPostgreSQL(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("live migration test skipped: TEST_DATABASE_URL for a dedicated PostgreSQL database is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open dedicated TEST_DATABASE_URL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("dedicated PostgreSQL at TEST_DATABASE_URL is unavailable: %v", err)
	}

	for filename := range serviceMigrations {
		t.Run(filename, func(t *testing.T) {
			base := regexp.MustCompile(`\W`).ReplaceAllString(strings.TrimSuffix(filename, ".sql"), "_")
			schema := fmt.Sprintf("task4_%s_%d", base, time.Now().UnixNano())
			if _, err := pool.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cleanupCancel()
				if _, err := pool.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
					t.Errorf("clean schema %s: %v", schema, err)
				}
			})

			migration := readMigration(t, filename)
			for pass := 1; pass <= 2; pass++ {
				connection, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, setErr := connection.Exec(ctx, `SET search_path TO `+schema)
				if setErr == nil {
					_, setErr = connection.Exec(ctx, migration)
				}
				connection.Release()
				if setErr != nil {
					t.Fatalf("apply pass %d: %v", pass, setErr)
				}
			}
			assertInvalidFixtureRejected(t, ctx, pool, schema, filename)
			assertDomainConstraintFixtures(t, ctx, pool, schema, filename)
		})
	}
}

func assertInvalidFixtureRejected(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, filename string) {
	t.Helper()
	fixtures := map[string]string{
		"catalog_db.sql":     `INSERT INTO variant (sku, product_id, title, attributes, price_minor, currency, weight_grams) VALUES ('bad', 'missing', 'bad', '{}', -1, 'CNY', 0)`,
		"customer_db.sql":    `INSERT INTO customer (customer_id, email, name, status, created_at) VALUES ('bad', 'bad@example.test', 'bad', 'unknown', now())`,
		"inventory_db.sql":   `INSERT INTO inventory_level (sku, location_id, on_hand, reserved, updated_at) VALUES ('bad', 'missing', 1, 2, now())`,
		"order_db.sql":       `INSERT INTO sales_order (order_id, order_no, customer_id, status, payment_status, fulfillment_status, currency, subtotal_minor, discount_minor, shipping_minor, tax_minor, total_minor, shipping_address, idempotency_key, placed_at) VALUES ('bad', 'bad', 'customer', 'pending', 'pending', 'unfulfilled', 'CNY', 100, 0, 0, 0, 99, '{}', 'bad', now())`,
		"payment_db.sql":     `INSERT INTO payment_intent (payment_intent_id, order_id, amount_minor, currency, status, provider, idempotency_key, created_at) VALUES ('bad', 'order', -1, 'CNY', 'requires_method', 'test', 'bad', now())`,
		"fulfillment_db.sql": `INSERT INTO fulfillment_order (fulfillment_id, order_id, location_id, status, created_at) VALUES ('bad', 'order', 'location', 'unknown', now())`,
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, fixtures[filename]); err == nil {
		t.Fatal(fmt.Sprintf("invalid %s fixture unexpectedly succeeded", filename))
	}
}

func assertDomainConstraintFixtures(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, filename string) {
	t.Helper()
	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatal(err)
	}

	switch filename {
	case "inventory_db.sql":
		if _, err := connection.Exec(ctx, `
			INSERT INTO location VALUES ('loc-generated', 'Generated Test', 'warehouse', 1);
			INSERT INTO inventory_level
			VALUES ('sku-generated', 'loc-generated', 10, 3, now())`); err != nil {
			t.Fatal(err)
		}
		var available int
		if err := connection.QueryRow(ctx, `
			SELECT available FROM inventory_level
			WHERE sku = 'sku-generated' AND location_id = 'loc-generated'`).Scan(&available); err != nil {
			t.Fatal(err)
		}
		if available != 7 {
			t.Fatalf("generated availability=%d, want 7", available)
		}
	case "order_db.sql":
		if _, err := connection.Exec(ctx, `
			INSERT INTO sales_order (
				order_id, order_no, customer_id, status, payment_status,
				fulfillment_status, currency, subtotal_minor, discount_minor,
				shipping_minor, tax_minor, total_minor, shipping_address,
				idempotency_key, placed_at
			) VALUES
				('order-a', 'NO-A', 'customer', 'confirmed', 'paid', 'unfulfilled',
				 'CNY', 100, 10, 0, 0, 90, '{}', 'order-a-key', now()),
				('order-b', 'NO-B', 'customer', 'confirmed', 'paid', 'unfulfilled',
				 'CNY', 100, 0, 0, 0, 100, '{}', 'order-b-key', now());
			INSERT INTO order_item VALUES
				('item-a', 'order-a', 'sku-a', 'A', 1, 100, 10, 90),
				('item-b', 'order-b', 'sku-b', 'B', 1, 100, 0, 100)`); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, ctx, connection, `
			INSERT INTO order_discount_allocation
				(allocation_id, order_id, order_item_id, source, amount_minor)
			VALUES ('bad-owner', 'order-a', 'item-b', 'promo', 10)`)
		if _, err := connection.Exec(ctx, `
			INSERT INTO order_discount_allocation
				(allocation_id, order_id, order_item_id, source, amount_minor)
			VALUES ('order-discount-1', 'order-a', NULL, 'promo', 10)`); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, ctx, connection, `
			INSERT INTO order_discount_allocation
				(allocation_id, order_id, order_item_id, source, amount_minor)
			VALUES ('order-discount-2', 'order-a', NULL, 'promo', 10)`)
		expectRejected(t, ctx, connection, `
			INSERT INTO sales_order (
				order_id, order_no, customer_id, status, payment_status,
				fulfillment_status, currency, subtotal_minor, discount_minor,
				shipping_minor, tax_minor, total_minor, shipping_address,
				idempotency_key, placed_at
			) VALUES ('bad-combination', 'NO-BAD', 'customer', 'completed', 'paid',
				'unfulfilled', 'CNY', 100, 0, 0, 0, 100, '{}', 'bad-combination', now())`)
		if _, err := connection.Exec(ctx, `
			INSERT INTO sales_order (
				order_id, order_no, customer_id, status, payment_status,
				fulfillment_status, currency, subtotal_minor, discount_minor,
				shipping_minor, tax_minor, total_minor, shipping_address,
				idempotency_key, placed_at
			) VALUES
				('cancelled-partial', 'NO-CP', 'customer', 'cancelled', 'refunded',
				 'partial', 'CNY', 100, 0, 0, 0, 100, '{}', 'cancelled-partial', now()),
				('completed-refunded', 'NO-CR', 'customer', 'completed', 'refunded',
				 'fulfilled', 'CNY', 100, 0, 0, 0, 100, '{}', 'completed-refunded', now())`); err != nil {
			t.Fatalf("legal compensation/return states rejected: %v", err)
		}
	case "payment_db.sql":
		if _, err := connection.Exec(ctx, `
			INSERT INTO payment_intent
				(payment_intent_id, order_id, amount_minor, currency, status, provider, idempotency_key, created_at)
			VALUES
				('pi-success', 'order-success', 100, 'CNY', 'succeeded', 'test', 'pi-success-key', now()),
				('pi-processing', 'order-processing', 100, 'CNY', 'processing', 'test', 'pi-processing-key', now())`); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, ctx, connection, `
			INSERT INTO payment_attempt VALUES
				('attempt-too-large', 'pi-success', 'succeeded', NULL, 101, now())`)
		expectRejected(t, ctx, connection, `
			INSERT INTO payment_attempt VALUES
				('attempt-no-code', 'pi-processing', 'failed', NULL, 100, now())`)
		expectRejected(t, ctx, connection, `
			INSERT INTO refund VALUES
				('refund-not-captured', 'pi-processing', 10, 'pending', 'test', 'refund-not-captured-key', now())`)
		if _, err := connection.Exec(ctx, `
			INSERT INTO refund VALUES
				('refund-60', 'pi-success', 60, 'pending', 'test', 'refund-60-key', now())`); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, ctx, connection, `
			INSERT INTO refund VALUES
				('refund-41', 'pi-success', 41, 'succeeded', 'test', 'refund-41-key', now())`)
		expectRejected(t, ctx, connection, `
			UPDATE payment_intent SET amount_minor = 99
			WHERE payment_intent_id = 'pi-success'`)
		expectRejected(t, ctx, connection, `
			UPDATE payment_intent SET status = 'failed'
			WHERE payment_intent_id = 'pi-success'`)
		expectRejected(t, ctx, connection, `
			UPDATE refund SET amount_minor = 101 WHERE refund_id = 'refund-60'`)
		expectRejected(t, ctx, connection, `
			UPDATE refund SET payment_intent_id = 'pi-processing'
			WHERE refund_id = 'refund-60'`)
		if _, err := connection.Exec(ctx, `
			INSERT INTO refund VALUES
				('refund-status', 'pi-success', 40, 'failed', 'test', 'refund-status-key', now())`); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, `
			UPDATE refund SET status = 'succeeded' WHERE refund_id = 'refund-status'`); err != nil {
			t.Fatalf("legal refund status update rejected: %v", err)
		}
		expectRejected(t, ctx, connection, `
			UPDATE payment_intent SET status = 'processing'
			WHERE payment_intent_id = 'pi-success'`)
		if _, err := connection.Exec(ctx, `
			UPDATE payment_intent SET status = 'refunded'
			WHERE payment_intent_id = 'pi-success'`); err != nil {
			t.Fatalf("forward status transition rejected: %v", err)
		}
		expectRejected(t, ctx, connection, `
			UPDATE payment_intent SET status = 'succeeded'
			WHERE payment_intent_id = 'pi-success'`)
		expectRejected(t, ctx, connection, `
			UPDATE payment_intent SET status = 'partially_refunded'
			WHERE payment_intent_id = 'pi-success'`)
		if _, err := connection.Exec(ctx, `
			INSERT INTO payment_intent
				(payment_intent_id, order_id, amount_minor, currency, status, provider, idempotency_key, created_at)
			VALUES ('pi-partial', 'order-partial', 100, 'CNY', 'succeeded', 'test', 'pi-partial-key', now());
			UPDATE payment_intent SET status = 'partially_refunded'
			WHERE payment_intent_id = 'pi-partial'`); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, ctx, connection, `
			UPDATE payment_intent SET status = 'succeeded'
			WHERE payment_intent_id = 'pi-partial'`)
	case "fulfillment_db.sql":
		if _, err := connection.Exec(ctx, `
			INSERT INTO fulfillment_order VALUES
				('fulfillment-valid', 'order-valid', 'location-valid', 'in_progress', now())`); err != nil {
			t.Fatal(err)
		}
		expectRejected(t, ctx, connection, `
			INSERT INTO shipment VALUES
				('shipment-bad', 'fulfillment-valid', 'carrier', 'tracking-bad', 'delivered', NULL, NULL)`)
	}
}

func expectRejected(t *testing.T, ctx context.Context, connection *pgxpool.Conn, statement string) {
	t.Helper()
	if _, err := connection.Exec(ctx, statement); err == nil {
		t.Fatalf("invalid fixture unexpectedly succeeded: %s", strings.TrimSpace(statement))
	}
}
