//go:build integration

package catalog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresCheckoutSnapshotResolvesActiveSKUAndRejectsInactive(t *testing.T) {
	store, pool := newIntegrationCatalogStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		INSERT INTO category (category_id, name, path)
		VALUES ('CAT-1', 'Category', '/Category');
		INSERT INTO product (
			product_id, title, description, brand, category_id, status, created_at
		) VALUES
			('PROD-1', 'Product', 'Description', 'Brand', 'CAT-1', 'active', $1),
			('PROD-2', 'Inactive', 'Description', 'Brand', 'CAT-1', 'inactive', $1);
		INSERT INTO variant (
			sku, product_id, title, attributes, price_minor, currency, weight_grams
		) VALUES
			('SKU-1', 'PROD-1', 'Black / S', '{"color":"black"}', 1236, 'CNY', 220),
			('SKU-2', 'PROD-2', 'Retired', '{}', 100, 'CNY', 100)`, now); err != nil {
		t.Fatal(err)
	}
	service := NewService(store)
	snapshot, err := service.GetCheckoutSnapshot(ctx, "SKU-1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ProductID != "PROD-1" || snapshot.UnitPriceMinor != 1236 ||
		!snapshot.AvailableForSale || snapshot.Attributes["color"] != "black" {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if _, err := service.GetCheckoutSnapshot(ctx, "SKU-2"); !errors.Is(err, ErrSKUNotSaleable) {
		t.Fatalf("inactive error=%v", err)
	}
	if _, err := service.GetCheckoutSnapshot(ctx, "MISSING"); !errors.Is(err, ErrSKUNotFound) {
		t.Fatalf("missing error=%v", err)
	}
}

func newIntegrationCatalogStore(t *testing.T) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("catalog integration test skipped: TEST_DATABASE_URL for a dedicated PostgreSQL database is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open dedicated TEST_DATABASE_URL: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("dedicated PostgreSQL at TEST_DATABASE_URL is unavailable: %v", err)
	}
	schema := fmt.Sprintf("task6_catalog_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "db", "migrations", "catalog_db.sql"))
	if err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Errorf("drop integration schema: %v", err)
		}
		admin.Close()
	})
	return NewPostgresStore(pool), pool
}
