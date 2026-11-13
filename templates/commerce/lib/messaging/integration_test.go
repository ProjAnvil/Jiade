//go:build integration

package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
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

func TestIntegrationConfirmedRawRouterRequiresRouteAndPositiveConfirmation(t *testing.T) {
	brokerURL := strings.TrimSpace(os.Getenv("TEST_AMQP_URL"))
	if brokerURL == "" {
		t.Skip("RabbitMQ integration dependency unavailable: TEST_AMQP_URL is not set")
	}
	connection, err := amqp.Dial(brokerURL)
	if err != nil {
		t.Fatalf("open TEST_AMQP_URL: %v", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	exchange := fmt.Sprintf("task6.confirmed.%d", time.Now().UnixNano())
	queue := exchange + ".queue"
	if err := channel.ExchangeDeclare(exchange, "direct", false, true, false, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.QueueDeclare(queue, false, true, false, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := channel.QueueBind(queue, "retry", exchange, false, nil); err != nil {
		t.Fatal(err)
	}
	router, err := NewConfirmedRouter(channel)
	if err != nil {
		t.Fatal(err)
	}
	defer router.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	message := amqp.Publishing{
		MessageId: "task6-confirmed-message", ContentType: "application/json",
		Body: []byte(`{"ok":true}`),
	}
	if err := router.Route(ctx, exchange, "retry", message); err != nil {
		t.Fatalf("confirmed route: %v", err)
	}
	delivery, ok, err := channel.Get(queue, true)
	if err != nil || !ok || delivery.MessageId != message.MessageId {
		t.Fatalf("routed delivery ok=%v id=%q err=%v", ok, delivery.MessageId, err)
	}
	if err := router.Route(ctx, exchange, "missing", amqp.Publishing{
		MessageId: "task6-unroutable", Body: []byte(`{}`),
	}); err == nil {
		t.Fatal("mandatory unroutable publish succeeded")
	}
}
