// Package main is the loan read-only API service entrance.
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
	"bank/internal/loan/api"
	"bank/internal/loan/repo"
	"bank/internal/loan/service"
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

	dbName := getenv("DB_NAME", "loan_db")
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
		Svc: service.NewLoanService(repo.NewLoanRepo(db, serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(customerConn)))),
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "loan",
		Instance: getenv("INSTANCE_ID", "loan-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	log.Printf("loan HTTP 监听 %s (db=%s)", httpAddr, dbName)
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
