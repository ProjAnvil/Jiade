// Package main is the reward read-only API service entrance.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	customerv1 "bank/gen/bank/customer/v1"
	"bank/internal/platform/grpcx"
	"bank/internal/platform/httpx"
	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
	"bank/internal/reward/api"
	"bank/internal/reward/repo"
	"bank/internal/reward/service"
)

func main() {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dbName := getenv("DB_NAME", "reward_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}
	customerConn, err := grpcx.Dial(signalCtx, grpcx.ClientConfig{Target: getenv("CUSTOMER_GRPC_TARGET", "dns:///customer:9090"), Timeout: 3 * time.Second})
	if err != nil {
		log.Fatalf("连接 customer gRPC 失败: %v", err)
	}
	defer customerConn.Close()

	handlers := &api.Handlers{
		Svc: service.NewRewardService(repo.NewRewardRepo(db, serviceclient.NewCustomerReader(customerv1.NewCustomerQueryServiceClient(customerConn)))),
	}
	httpAddr := getenv("HTTP_ADDR", ":8080")
	srv := httpx.NewServer(httpx.ServerConfig{
		Service:  "reward",
		Instance: getenv("INSTANCE_ID", "reward-1"),
		Addr:     httpAddr,
		Handler:  api.NewRouter(handlers),
		Ready:    func(ctx context.Context) error { return db.PingContext(ctx) },
	})

	go func() {
		log.Printf("reward HTTP 监听 %s (db=%s)", httpAddr, dbName)
		if err := srv.ListenAndServe(); err != nil && !httpx.IsClosed(err) {
			log.Printf("reward HTTP 服务停止: %v", err)
			stop()
		}
	}()

	<-signalCtx.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
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
