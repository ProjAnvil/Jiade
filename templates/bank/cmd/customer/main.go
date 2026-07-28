// Package main is the customer read-only API service entry.
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
	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/customer/api"
	"bank/internal/customer/repo"
	"bank/internal/customer/service"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
)

func main() {
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
	coreConn, err := grpcx.Dial(context.Background(), grpcx.ClientConfig{Target: getenv("CORE_BANKING_GRPC_TARGET", "dns:///core-banking:9090"), Timeout: 3 * time.Second})
	if err != nil {
		log.Fatalf("连接 core-banking gRPC 失败: %v", err)
	}
	defer coreConn.Close()

	customerRepo := repo.NewCustomerRepo(db, serviceclient.NewAccountReader(corev1.NewAccountQueryServiceClient(coreConn)))
	handlers := &api.Handlers{Svc: service.NewCustomerService(customerRepo)}
	port := getenv("API_PORT", "8080")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	grpcServer := grpcx.NewServer(grpcx.ServerConfig{Ready: func(ctx context.Context) error { return db.PingContext(ctx) }})
	customerv1.RegisterCustomerQueryServiceServer(grpcServer, api.NewCustomerQueryServer(customerRepo))
	grpcListener, err := net.Listen("tcp", ":"+getenv("GRPC_PORT", "9090"))
	if err != nil {
		log.Fatalf("customer gRPC 监听失败: %v", err)
	}

	go func() {
		log.Printf("customer 监听 :%s (db=%s)", port, dbName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	go func() {
		if err := grpcServer.Serve(grpcListener); err != nil {
			log.Printf("customer gRPC 服务停止: %v", err)
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
