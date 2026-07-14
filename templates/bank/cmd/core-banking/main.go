// Package main 是 core-banking 只读 API 服务入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank/internal/corebanking/api"
	"bank/internal/corebanking/repo"
	"bank/internal/corebanking/service"
	"bank/internal/platform/pg"
)

func main() {
	dbName := getenv("DB_NAME", "core_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()

	// 启动重试：core_db 可能尚未就绪（seed 未跑完）
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}

	handlers := &api.Handlers{
		Accounts: repo.NewAccountRepo(db),
		TxnSvc:   service.NewTxnService(repo.NewTxnRepo(db)),
		Ledger:   repo.NewLedgerRepo(db),
	}
	port := getenv("API_PORT", "8080")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	go func() {
		log.Printf("core-banking 监听 :%s (db=%s)", port, dbName)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
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
