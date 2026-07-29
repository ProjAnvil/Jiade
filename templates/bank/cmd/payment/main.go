// Package main is the payment read-only API service entry.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "bank/gen/bank/core/v1"
	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/payment/api"
	"bank/internal/payment/repo"
	"bank/internal/payment/service"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/pg"
	"bank/internal/platform/runx"
	"bank/internal/platform/serviceclient"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbName := getenv("DB_NAME", "pay_db")
	db, err := pg.Open(dbName)
	if err != nil {
		return fmt.Errorf("打开 %s 失败: %w", dbName, err)
	}
	defer db.Close()

	// Start retry: pay_db may not be ready yet (seed has not finished running)
	if err := waitForDB(db, 5, time.Second); err != nil {
		return fmt.Errorf("连 %s 失败（请先 make up 再 make seed）: %w", dbName, err)
	}
	coreConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CORE_BANKING_GRPC_TARGET", "dns:///core-banking:9090"), Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("连接 core-banking gRPC 失败: %w", err)
	}
	defer coreConn.Close()
	customerConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CUSTOMER_GRPC_TARGET", "dns:///customer:9090"), Timeout: 3 * time.Second})
	if err != nil {
		return fmt.Errorf("连接 customer gRPC 失败: %w", err)
	}
	defer customerConn.Close()

	handlers := &api.Handlers{
		Svc: service.NewPaymentService(repo.NewPaymentRepo(db,
			serviceclient.NewAccountReader(corev1.NewAccountQueryServiceClient(coreConn)),
			serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(customerConn)))),
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "payment",
		Instance: getenv("INSTANCE_ID", "payment-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	log.Printf("payment HTTP 监听 %s (db=%s)", httpAddr, dbName)
	return runx.Serve(signalCtx, srv, nil, 5*time.Second)
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
