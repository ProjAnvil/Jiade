// Package main is the risk service entry: read-only HTTP API + the saga
// command consumer (risk.authorize-payment.v1 / risk.void-payment-authorization.v1)
// + the outbox relay that drains result events the consumer wrote and publishes
// them to bank.events.
//
// Composition (Task 6):
//   - HTTP server: /api/v1/risk/... read-only routes + /readyz, /livez, /metrics.
//   - Command consumer: subscribes to the risk.commands queue and feeds each
//     envelope to AuthorizationService, writing the result event to the outbox
//     in the same tx.
//   - Outbox relay: drains outbox_message rows the consumer wrote (result
//     events risk.payment.*, risk.command.rejected) and publishes them to the
//     bank.events topic exchange.
//
// All three (HTTP, consumer, relay) run under runx.Serve; if any one exits, the
// internal context is cancelled and the process shuts down. The /readyz probe
// pings the DB — a worker crash surfaces as a process restart by the
// orchestrator.
package main

import (
	"context"
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
	"bank/internal/risk"
	"bank/internal/risk/api"
	"bank/internal/risk/repo"
	"bank/internal/risk/service"

	amqp "github.com/rabbitmq/amqp091-go"
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
	telemetryProvider := initTelemetry(signalCtx, "risk", getenv("INSTANCE_ID", "risk-1"))
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := telemetryProvider.Shutdown(shutdownCtx); err != nil {
			log.Printf("risk telemetry shutdown: %v", err)
		}
		cancel()
	}()

	dbName := getenv("DB_NAME", "risk_db")
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
	customerReader := serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(customerConn))

	handlers := &api.Handlers{
		Svc: service.NewRiskService(repo.NewRiskRepo(db, customerReader)),
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "risk",
		Instance: getenv("INSTANCE_ID", "risk-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	// --- Saga command consumer + outbox relay ---
	amqpURL := getenv("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	commandQueue := getenv("RISK_COMMAND_QUEUE", "risk.commands")
	// Retry/DLQ routing keys MUST match definitions.json: bank.retry →
	// risk.commands.retry (TTLs back to bank.commands with routing key
	// risk.commands) and bank.dlx → risk.commands.dead.
	retryPolicy := messaging.RetryPolicy{
		MaxAttempts:          3,
		RetryExchange:        getenv("RISK_RETRY_EXCHANGE", messaging.ExchangeRetry),
		RetryRoutingKey:      getenv("RISK_RETRY_KEY", "risk.commands.retry"),
		DeadLetterExchange:   getenv("RISK_DLQ_EXCHANGE", messaging.ExchangeDeadLetter),
		DeadLetterRoutingKey: getenv("RISK_DLQ_KEY", "risk.commands.dead"),
	}
	authRepo := repo.NewAuthorizationRepo()
	authSvc := service.NewAuthorizationService(authRepo, customerReader, nil)
	riskOutbox := risk.NewRiskOutbox(db)
	consumer := risk.NewConsumer(db, authSvc, riskOutbox, retryPolicy, nil)

	// Outbox relay: eagerly dial the broker so a broker-down condition fails
	// the process at startup rather than silently skipping event delivery.
	// The relay derives the topic exchange per outbox row via
	// messaging.ExchangeForRoutingKey, so the publisher's constructor exchange
	// is never used for the relay's own publishes. bank.events is the
	// documented default because risk's outbox holds result events
	// (risk.payment.*, risk.command.rejected).
	relayConn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("risk outbox relay 连接 broker 失败: %w", err)
	}
	defer relayConn.Close()
	relayCh, err := relayConn.Channel()
	if err != nil {
		return fmt.Errorf("risk outbox relay 打开 channel 失败: %w", err)
	}
	relayPublisher, err := messaging.NewRabbitPublisher(relayCh, messaging.ExchangeEvents)
	if err != nil {
		return fmt.Errorf("risk outbox relay publisher 失败: %w", err)
	}
	relay := risk.NewOutboxRelay(db, relayPublisher)

	// Workers: run under the signal context via runx.Serve. If either stops
	// unexpectedly, runx.Serve cancels the context so HTTP shuts down too.
	workers := []runx.Worker{
		runx.WorkerFunc(func(ctx context.Context) error {
			return consumer.Run(ctx, amqpURL, commandQueue)
		}),
		runx.WorkerFunc(func(ctx context.Context) error {
			defer relayPublisher.Close()
			return relay.Run(ctx)
		}),
	}

	log.Printf("risk HTTP 监听 %s (db=%s), 消费队列=%s", httpAddr, dbName, commandQueue)
	return runx.Serve(signalCtx, srv, nil, 5*time.Second, workers...)
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
