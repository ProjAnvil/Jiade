//go:build integration

package messaging

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestIntegrationHandleOnceDuplicateDeliveryMutatesDatabaseOnce(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PostgreSQL integration dependency unavailable: TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open TEST_DATABASE_URL: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("PostgreSQL at TEST_DATABASE_URL is unavailable: %v", err)
	}

	schema, err := os.ReadFile(filepath.Join("..", "..", "..", "db", "migrations", "shared.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply shared schema: %v", err)
	}

	connection, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `CREATE TEMP TABLE messaging_projection_test (event_id uuid PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	event := NewEvent("order.placed.v1", "ORD-integration", "corr-integration", "", json.RawMessage(`{"total_minor":1200}`), func() time.Time { return time.Now().UTC() })
	for range 2 {
		tx, err := connection.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = HandleOnce(ctx, tx, "integration-projection", event, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO messaging_projection_test (event_id) VALUES ($1)`, event.ID)
			return err
		})
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	var mutations, inboxRows int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM messaging_projection_test`).Scan(&mutations); err != nil {
		t.Fatal(err)
	}
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM inbox_event WHERE consumer = $1 AND event_id = $2`, "integration-projection", event.ID).Scan(&inboxRows); err != nil {
		t.Fatal(err)
	}
	if mutations != 1 || inboxRows != 1 {
		t.Fatalf("mutations=%d inboxRows=%d, want 1 and 1", mutations, inboxRows)
	}
}
