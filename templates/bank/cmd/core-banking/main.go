// Package main is the core-banking API service entrance (read-only query + B-3 accounting/rewriting interface).
package main

import (
	"context"
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
)

func main() {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbName := getenv("DB_NAME", "core_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()

	// Start retry: core_db may not be ready yet (seed has not finished running)
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
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
		log.Fatalf("core-banking gRPC 监听失败: %v", err)
	}

	go func() {
		log.Printf("core-banking HTTP 监听 %s (db=%s)", httpAddr, dbName)
		if err := srv.ListenAndServe(); err != nil && !httpx.IsClosed(err) {
			log.Printf("core-banking HTTP 服务停止: %v", err)
			stop()
		}
	}()
	go func() {
		log.Printf("core-banking gRPC 监听 %s", grpcAddr)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Printf("core-banking gRPC 服务停止: %v", err)
			stop()
		}
	}()

	<-signalCtx.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	grpcx.Shutdown(ctx, grpcServer)
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
