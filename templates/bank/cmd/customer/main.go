// Package main is the customer read-only API service entry.
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
	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/customer/api"
	"bank/internal/customer/repo"
	"bank/internal/customer/service"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
)

func main() {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbName := getenv("DB_NAME", "cust_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()

	// Start retry: cust_db may not be ready yet (seed has not finished running)
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}
	coreConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CORE_BANKING_GRPC_TARGET", "dns:///core-banking:9090"), Timeout: 3 * time.Second})
	if err != nil {
		log.Fatalf("连接 core-banking gRPC 失败: %v", err)
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
		log.Fatalf("customer gRPC 监听失败: %v", err)
	}

	go func() {
		log.Printf("customer HTTP 监听 %s (db=%s)", httpAddr, dbName)
		if err := srv.ListenAndServe(); err != nil && !httpx.IsClosed(err) {
			log.Printf("customer HTTP 服务停止: %v", err)
			stop()
		}
	}()
	go func() {
		log.Printf("customer gRPC 监听 %s", grpcAddr)
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Printf("customer gRPC 服务停止: %v", err)
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
