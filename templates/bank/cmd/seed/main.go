// Package main 是 bank 工程 fixture 生成器入口。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"bank/internal/fixtures"
	"bank/internal/fixtures/domains"
	"bank/internal/platform/migrate"
	"bank/internal/platform/pg"
)

func main() {
	scale := flag.String("scale", "dev", "规模：dev|full")
	reset := flag.Bool("reset", false, "重建库与表（幂等）")
	flag.Parse()

	cfg := fixtures.DefaultConfig(fixtures.Scale(*scale))
	log.Printf("[seed] scale=%s biz_date=%s~%s seed=%d reset=%v",
		*scale, cfg.StartBizDate, cfg.EndBizDate, cfg.Seed, *reset)

	ctx := context.Background()

	log.Println("[seed] 1/4 建库")
	if err := ensureDB(ctx, *reset); err != nil {
		log.Fatalf("建库失败: %v（请先 make up 启动 postgres）", err)
	}

	log.Println("[seed] 2/4 建表")
	db, err := pg.Open("core_db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ddl, err := os.ReadFile("db/migrations/core_db.sql")
	if err != nil {
		log.Fatalf("读 core_db.sql 失败: %v（请在工程根目录运行）", err)
	}
	if err := migrate.Run(ctx, db, string(ddl)); err != nil {
		log.Fatalf("建表失败: %v", err)
	}

	log.Println("[seed] 3/4 生成 + 灌数据")
	demand, fixed := domains.GenAccountRows(cfg)
	demandNos := make([]string, len(demand))
	for i, d := range demand {
		demandNos[i] = d.AccountNo
	}
	balances := domains.GenBalanceRows(cfg, demandNos)
	txns := domains.GenTxnRows(cfg, demandNos)

	if err := domains.WriteStatic(ctx, db, domains.GenStaticData(cfg)); err != nil {
		log.Fatal(err)
	}
	if err := domains.WriteAccounts(ctx, db, demand, fixed); err != nil {
		log.Fatal(err)
	}
	if err := domains.WriteBalances(ctx, db, balances); err != nil {
		log.Fatal(err)
	}
	if err := domains.WriteTxns(ctx, db, txns); err != nil {
		log.Fatal(err)
	}

	log.Printf("[seed] 4/4 完成 ✅ 活期 %d 定期 %d 余额 %d 流水 %d",
		len(demand), len(fixed), len(balances), len(txns))
}

// ensureDB 确保 core_db 存在；reset 时先 DROP 再 CREATE。连不上时短暂重试。
func ensureDB(ctx context.Context, reset bool) error {
	var admin *sql.DB
	var err error
	for i := 0; i < 5; i++ {
		admin, err = pg.Open("postgres")
		if err == nil {
			err = admin.Ping()
		}
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("连 postgres 管理库: %w", err)
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname='core_db')").Scan(&exists); err != nil {
		return err
	}
	if exists && reset {
		admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='core_db' AND pid<>pg_backend_pid()")
		if _, err := admin.ExecContext(ctx, "DROP DATABASE core_db"); err != nil {
			return err
		}
		exists = false
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE core_db"); err != nil {
			return err
		}
	}
	return nil
}
