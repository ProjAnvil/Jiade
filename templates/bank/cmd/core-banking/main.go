// Package main is the core-banking API service entrance (read-only query +
// B-3 accounting/rewriting interface + saga command consumer + outbox relay).
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
	"bank/internal/corebanking"
	"bank/internal/corebanking/api"
	"bank/internal/corebanking/repo"
	"bank/internal/corebanking/service"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/runx"

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

	dbName := getenv("DB_NAME", "core_db")
	db, err := pg.Open(dbName)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", dbName, err)
	}
	defer db.Close()

	// Start retry: core_db may not be ready yet (seed has not finished running)
	if err := waitForDB(db, 5, time.Second); err != nil {
		return fmt.Errorf("连 %s 失败（请先 make up 再 make seed）: %w", dbName, err)
	}

	ledgerRepo := repo.NewLedgerRepo(db)
	ledgerSvc := service.NewLedgerService(ledgerRepo)
	holdRepo := repo.NewHoldRepo(db)
	accountRepo := repo.NewAccountRepo(db)
	txnRepo := repo.NewTxnRepo(db)
	txnSvc := service.NewTxnService(db, accountRepo, ledgerSvc, ledgerRepo).WithReader(txnRepo)
	holdSvc := service.NewHoldService(db, holdRepo)
	transferSvc := service.NewHeldTransferService(db, holdRepo, accountRepo, ledgerSvc, ledgerRepo, ledgerRepo)

	handlers := &api.Handlers{
		Accounts: accountRepo,
		TxnSvc:   txnSvc,
		Ledger:   ledgerRepo,
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")

	// Readiness: the HTTP /readyz probe pings the DB. If the consumer or relay
	// stops unexpectedly, runx.Serve cancels the context and returns an error
	// — the process exits and the orchestrator restarts it.
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "core-banking",
		Instance: getenv("INSTANCE_ID", "core-banking-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	grpcServer := grpcx.NewServer(grpcx.ServerConfig{Ready: func(ctx context.Context) error { return db.PingContext(ctx) }})
	corev1.RegisterAccountQueryServiceServer(grpcServer, api.NewAccountQueryServer(accountRepo, txnRepo))
	grpcAddr := getenv("GRPC_ADDR", ":9090")
	grpcListener, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return fmt.Errorf("core-banking gRPC 监听失败: %w", err)
	}

	// --- Saga command consumer + outbox relay ---
	amqpURL := getenv("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	commandQueue := getenv("CORE_BANKING_COMMAND_QUEUE", "core-banking.commands")
	retryPolicy := messaging.RetryPolicy{
		MaxAttempts:          3,
		RetryRoutingKey:      getenv("CORE_BANKING_RETRY_KEY", "core-banking.retry"),
		DeadLetterRoutingKey: getenv("CORE_BANKING_DLQ_KEY", "core-banking.dlq"),
	}
	consumer := corebanking.NewConsumer(db, holdSvc, transferSvc, ledgerRepo, retryPolicy, nil)

	// Outbox relay: eagerly dial the broker so a broker-down condition fails
	// the process at startup rather than silently skipping event delivery.
	relayConn, err := amqp.Dial(amqpURL)
	if err != nil {
		return fmt.Errorf("core-banking outbox relay 连接 broker 失败: %w", err)
	}
	defer relayConn.Close()
	relayCh, err := relayConn.Channel()
	if err != nil {
		return fmt.Errorf("core-banking outbox relay 打开 channel 失败: %w", err)
	}
	relayPublisher, err := messaging.NewRabbitPublisher(relayCh, "")
	if err != nil {
		return fmt.Errorf("core-banking outbox relay publisher 失败: %w", err)
	}
	relay := corebanking.NewOutboxRelay(db, relayPublisher)

	// Workers: run under the signal context via runx.Serve. If either stops
	// unexpectedly, runx.Serve cancels the context so HTTP/gRPC shut down too.
	workers := []runx.Worker{
		runx.WorkerFunc(func(ctx context.Context) error {
			return consumer.Run(ctx, amqpURL, commandQueue)
		}),
		runx.WorkerFunc(func(ctx context.Context) error {
			defer relayPublisher.Close()
			return relay.Run(ctx)
		}),
	}

	log.Printf("core-banking HTTP 监听 %s, gRPC 监听 %s (db=%s), 消费队列=%s", httpAddr, grpcAddr, dbName, commandQueue)
	return runx.Serve(signalCtx, srv, &runx.GRPCService{
		Server:   grpcServer,
		Listener: grpcListener,
	}, 5*time.Second, workers...)
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
