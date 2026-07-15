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

// 所有业务库与其迁移 SQL（顺序无关，建库建表幂等）。
var allDBs = []struct{ name, sql string }{
	{"core_db", "db/migrations/core_db.sql"},
	{"cust_db", "db/migrations/cust_db.sql"},
	{"pay_db", "db/migrations/pay_db.sql"},
}

func main() {
	scale := flag.String("scale", "dev", "规模：dev|full")
	reset := flag.Bool("reset", false, "重建库与表（幂等）")
	flag.Parse()

	cfg := fixtures.DefaultConfig(fixtures.Scale(*scale))
	log.Printf("[seed] scale=%s biz_date=%s~%s seed=%d reset=%v",
		*scale, cfg.StartBizDate, cfg.EndBizDate, cfg.Seed, *reset)

	ctx := context.Background()

	log.Println("[seed] 1/6 建 3 库")
	names := make([]string, len(allDBs))
	for i, d := range allDBs {
		names[i] = d.name
	}
	if err := ensureDBs(ctx, *reset, names); err != nil {
		log.Fatalf("建库失败: %v（请先 make up 启动 postgres）", err)
	}

	log.Println("[seed] 2/6 建 3 库表")
	for _, d := range allDBs {
		db, err := pg.Open(d.name)
		if err != nil {
			log.Fatal(err)
		}
		ddl, err := os.ReadFile(d.sql)
		if err != nil {
			log.Fatalf("读 %s 失败: %v（请在工程根目录运行）", d.sql, err)
		}
		if err := migrate.Run(ctx, db, string(ddl)); err != nil {
			log.Fatalf("建表 %s 失败: %v", d.name, err)
		}
		db.Close()
	}

	log.Println("[seed] 3/6 core 生成 + 灌数据")
	coreDB, err := pg.Open("core_db")
	if err != nil {
		log.Fatal(err)
	}
	defer coreDB.Close()
	demand, fixed := domains.GenAccountRows(cfg)
	demandNos := make([]string, len(demand))
	for i, d := range demand {
		demandNos[i] = d.AccountNo
	}
	balances := domains.GenBalanceRows(cfg, demandNos)
	txns := domains.GenTxnRows(cfg, demandNos)
	if err := domains.WriteStatic(ctx, coreDB, domains.GenStaticData(cfg)); err != nil {
		log.Fatal(err)
	}
	if err := domains.WriteAccounts(ctx, coreDB, demand, fixed); err != nil {
		log.Fatal(err)
	}
	if err := domains.WriteBalances(ctx, coreDB, balances); err != nil {
		log.Fatal(err)
	}
	if err := domains.WriteTxns(ctx, coreDB, txns); err != nil {
		log.Fatal(err)
	}
	log.Printf("[seed] core: 活期 %d 定期 %d 余额 %d 流水 %d",
		len(demand), len(fixed), len(balances), len(txns))

	log.Println("[seed] 4/6 customer 域（占位，Task 12 接入）")
	log.Println("[seed] 5/6 payment 域（占位，Task 12 接入）")
	log.Println("[seed] 6/6 setup_fdw（占位，Task 12 接入）")
	log.Println("[seed] 完成 ✅（core；customer/payment/fdw 待 Task 12 接入）")
}

// ensureDBs 确保 names 中的库都存在；reset 时先 DROP 再 CREATE。连不上时短暂重试。
func ensureDBs(ctx context.Context, reset bool, names []string) error {
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

	for _, db := range names {
		var exists bool
		if err := admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", db).Scan(&exists); err != nil {
			return err
		}
		if exists && reset {
			admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", db)
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE "%s"`, db)); err != nil {
				return err
			}
			exists = false
		}
		if !exists {
			if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE "%s"`, db)); err != nil {
				return err
			}
		}
	}
	return nil
}
