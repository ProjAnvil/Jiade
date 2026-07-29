// Package main is the core-banking API service entrance (read-only query + B-3 accounting/rewriting interface).
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
	"bank/internal/corebanking/api"
	"bank/internal/corebanking/repo"
	"bank/internal/corebanking/service"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/pg"
	"bank/internal/platform/runx"
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
	txnRepo := repo.NewTxnRepo(db)
	accountRepo := repo.NewAccountRepo(db)
	txnSvc := service.NewTxnService(db, accountRepo, ledgerSvc, ledgerRepo).WithReader(txnRepo)

	handlers := &api.Handlers{
		Accounts: accountRepo,
		TxnSvc:   txnSvc,
		Ledger:   ledgerRepo,
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")
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

	log.Printf("core-banking HTTP 监听 %s, gRPC 监听 %s (db=%s)", httpAddr, grpcAddr, dbName)
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
