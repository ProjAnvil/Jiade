// Package main is the reward service entrance: read-only HTTP API + the
// payment-completion consumer that earns points on a successful payment.
//
// Composition (Task 8):
//   - HTTP server: /api/v1/points/... /api/v1/coupons/... read-only routes.
//   - Result-event consumer: subscribes to the reward.events queue, consumes
//     payment.completed.v1, and earns points for the payer. A NON-CRITICAL
//     consumer: failures route to the reward DLQ and never affect payment
//     status.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/runx"
	"bank/internal/platform/serviceclient"
	"bank/internal/platform/telemetry"
	"bank/internal/reward"
	"bank/internal/reward/api"
	"bank/internal/reward/repo"
	"bank/internal/reward/service"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Initialise OpenTelemetry tracing early so the otelgrpc/otelhttp
	// instrumentation installed in Task 2 exports spans via the global
	// TracerProvider. OTel init failure MUST NOT block startup: on error we
	// fall back to a NoOp provider so the process keeps serving traffic.
	telemetryProvider := initTelemetry(signalCtx, "reward", getenv("INSTANCE_ID", "reward-1"))
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := telemetryProvider.Shutdown(shutdownCtx); err != nil {
			log.Printf("reward telemetry shutdown: %v", err)
		}
		cancel()
	}()

	dbName := getenv("DB_NAME", "reward_db")
	db, err := pg.Open(dbName)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		return fmt.Errorf("连 %s 失败（请先 make up 再 make seed）: %w", dbName, err)
	}
	customerConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CUSTOMER_GRPC_TARGET", "dns:///customer:9090"), Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("连接 customer gRPC 失败: %w", err)
	}
	defer customerConn.Close()

	handlers := &api.Handlers{
		Svc: service.NewRewardService(repo.NewRewardRepo(db, serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(customerConn)))),
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "reward",
		Instance: getenv("INSTANCE_ID", "reward-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	// Task 8: payment-completion consumer. Subscribes to payment.completed.v1
	// and earns points for the payer. Failures route to reward DLQ and never
	// affect payment status.
	amqpURL := getenv("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	eventQueue := getenv("REWARD_EVENT_QUEUE", "reward.events")
	retryPolicy := messaging.RetryPolicy{
		MaxAttempts:          3,
		RetryRoutingKey:      getenv("REWARD_RETRY_KEY", "reward.retry"),
		DeadLetterRoutingKey: getenv("REWARD_DLQ_KEY", "reward.dlq"),
	}
	consumer := reward.NewConsumer(db, &pointsEarner{db: db}, retryPolicy)

	workers := []runx.Worker{
		runx.WorkerFunc(func(ctx context.Context) error {
			return consumer.Run(ctx, amqpURL, eventQueue)
		}),
	}

	log.Printf("reward HTTP 监听 %s (db=%s), 事件队列=%s", httpAddr, dbName, eventQueue)
	return runx.Serve(signalCtx, srv, nil, 5*time.Second, workers...)
}

// pointsEarner is the concrete reward.PointsEarner: it records a points-earn
// txn against the payer's points_acct. A real implementation would compute
// earn rates by member level, check campaign eligibility, etc.; this minimal
// implementation records a flat 1 point per minor unit so the wiring is
// end-to-end functional. Returning an error causes the consumer to retry and
// eventually DLQ the delivery — payment status is never affected.
type pointsEarner struct {
	db *sql.DB
}

func (e *pointsEarner) EarnPoints(ctx context.Context, paymentID, customerID string, amountMinor int64, currency string) error {
	if customerID == "" || amountMinor <= 0 {
		return fmt.Errorf("reward: cannot earn points for payment %s (customer=%s amount=%d)", paymentID, customerID, amountMinor)
	}
	// Minimal earn: 1 point per minor unit. A real implementation would
	// compute based on member level, campaign, currency conversion, etc.
	points := int(amountMinor)
	_, err := e.db.ExecContext(ctx,
		`UPDATE points_acct SET points_balance = points_balance + $2, update_biz_date = CURRENT_DATE WHERE cust_id = $1`,
		customerID, points)
	if err != nil {
		return fmt.Errorf("reward: earn points for %s: %w", customerID, err)
	}
	return nil
}

type pinger interface{ Ping() error }

func waitForDB(p pinger, retries int, wait time.Duration) error {
	var err error
	for i := 0; i < retries; i++ {
		if err = p.Ping(); err == nil {
			return nil
		}
		time.Sleep(wait)
	}
	return err
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// initTelemetry configures the global OpenTelemetry TracerProvider from the
// OTEL_* env vars set by compose.observability.yaml. provider.New /
// provider.Disabled install the provider globally (otel.SetTracerProvider +
// otel.SetTextMapPropagator), so the otelgrpc/otelhttp instrumentation
// installed in Task 2 picks it up automatically. On init failure it installs a
// NoOp provider so OTel problems never block startup.
func initTelemetry(ctx context.Context, service, instance string) *telemetry.Provider {
	cfg := telemetry.Config{
		Service:  service,
		Instance: instance,
		Endpoint: os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Enabled:  os.Getenv("OTEL_ENABLED") == "true",
		Insecure: os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true",
	}
	provider, err := telemetry.New(ctx, cfg)
	if err != nil {
		log.Printf("%s: telemetry init 失败，降级到 NoOp provider: %v", service, err)
		return telemetry.Disabled()
	}
	return provider
}
