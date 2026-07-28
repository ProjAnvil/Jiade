// Package main is the core-banking API service entrance (read-only query + B-3 accounting/rewriting interface).
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "bank/gen/bank/core/v1"
	"bank/internal/corebanking/api"
	"bank/internal/corebanking/repo"
	"bank/internal/corebanking/service"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/pg"
)

func main() {
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
	port := getenv("API_PORT", "8080")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	grpcServer := grpcx.NewServer(grpcx.ServerConfig{Ready: func(ctx context.Context) error { return db.PingContext(ctx) }})
	corev1.RegisterAccountQueryServiceServer(grpcServer, api.NewAccountQueryServer(accountRepo, txnRepo))
	grpcListener, err := net.Listen("tcp", ":"+getenv("GRPC_PORT", "9090"))
	if err != nil {
		log.Fatalf("core-banking gRPC 监听失败: %v", err)
	}

	go func() {
		log.Printf("core-banking 监听 :%s (db=%s)", port, dbName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	go func() {
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Printf("core-banking gRPC 服务停止: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
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
