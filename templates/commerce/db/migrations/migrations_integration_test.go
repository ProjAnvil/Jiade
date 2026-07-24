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
