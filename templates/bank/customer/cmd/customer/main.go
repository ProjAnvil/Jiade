// Package main is the customer read-only API service entry.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "bank/gen/bank/core/v1"
	customerv1 "bank/gen/bank/customer/v1"
	"bank/customer/customer/api"
	"bank/customer/customer/repo"
	"bank/customer/customer/service"
	"bank/lib/grpcx"
	"bank/lib/httpx"
	"bank/lib/pg"
	"bank/lib/runx"
	"bank/lib/serviceclient"
	"bank/lib/telemetry"
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
	telemetryProvider := initTelemetry(signalCtx, "customer", getenv("INSTANCE_ID", "customer-1"))
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := telemetryProvider.Shutdown(shutdownCtx); err != nil {
			log.Printf("customer telemetry shutdown: %v", err)
		}
		cancel()
	}()

	dbName := getenv("DB_NAME", "cust_db")
	db, err := pg.Open(dbName)
	if err != nil {
		return fmt.Errorf("open %s failed: %w", dbName, err)
	}
	defer db.Close()

	// Start retry: cust_db may not be ready yet (seed has not finished running)
	if err := waitForDB(db, 5, time.Second); err != nil {
		return fmt.Errorf("connect %s failed (run make up then make seed first): %w", dbName, err)
	}
	coreConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CORE_BANKING_GRPC_TARGET", "dns:///core-banking:9090"), Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("connect core-banking gRPC failed: %w", err)
	}
	defer coreConn.Close()

	customerRepo := repo.NewCustomerRepo(db, serviceclient.NewAccountReader(corev1.NewAccountQueryServiceClient(coreConn)))
	handlers := &api.Handlers{Svc: service.NewCustomerService(customerRepo)}
	httpAddr := getenv("HTTP_ADDR", ":8080")
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "customer",
		Instance: getenv("INSTANCE_ID", "customer-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	grpcServer := grpcx.NewServer(grpcx.ServerConfig{Ready: func(ctx context.Context) error { return db.PingContext(ctx) }})
	customerv1.RegisterCustomerQueryServiceServer(grpcServer, api.NewCustomerQueryServer(customerRepo))
	grpcAddr := getenv("GRPC_ADDR", ":9090")
	grpcListener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("customer gRPC listen failed: %w", err)
	}

	log.Printf("customer HTTP listening %s, gRPC listening %s (db=%s)", httpAddr, grpcAddr, dbName)
	return runx.Serve(signalCtx, srv, &runx.GRPCService{
		Server:   grpcServer,
		Listener: grpcListener,
	}, 5*time.Second)
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
		log.Printf("%s: telemetry init failed, falling back to NoOp provider: %v", service, err)
		return telemetry.Disabled()
	}
	return provider
}
