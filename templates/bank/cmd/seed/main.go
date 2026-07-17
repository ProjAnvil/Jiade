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
	"bank/internal/platform/fdw"
	"bank/internal/platform/migrate"
	"bank/internal/platform/pg"
)

var allDBs = []struct{ name, sql string }{
	{"core_db", "db/migrations/core_db.sql"},
	{"cust_db", "db/migrations/cust_db.sql"},
	{"pay_db", "db/migrations/pay_db.sql"},
	{"reward_db", "db/migrations/reward_db.sql"},
	{"risk_db", "db/migrations/risk_db.sql"},
	{"loan_db", "db/migrations/loan_db.sql"},
	{"wealth_db", "db/migrations/wealth_db.sql"},
}

func main() {
	scale := flag.String("scale", "dev", "规模：dev|full")
	reset := flag.Bool("reset", false, "重建库与表（幂等）")
	flag.Parse()
	cfg := fixtures.DefaultConfig(fixtures.Scale(*scale))
	log.Printf("[seed] scale=%s biz_date=%s~%s seed=%d reset=%v",
		*scale, cfg.StartBizDate, cfg.EndBizDate, cfg.Seed, *reset)
	if err := runSeed(context.Background(), cfg, *reset); err != nil {
		log.Fatalf("[seed] 失败: %v", err)
	}
	log.Println("[seed] 完成 ✅（7 库 + core + customer + payment + reward + risk + loan + wealth + FDW）")
}

func runSeed(ctx context.Context, cfg fixtures.Config, reset bool) error {
	names := make([]string, len(allDBs))
	for i, d := range allDBs {
		names[i] = d.name
	}
	log.Println("[seed] 1/10 建 7 库")
	if err := ensureDBs(ctx, reset, names); err != nil {
		return fmt.Errorf("建库: %w（请先 make up）", err)
	}
	log.Println("[seed] 2/10 建 7 库表")
	for _, d := range allDBs {
		db, err := pg.Open(d.name)
		if err != nil {
			return err
		}
		ddl, err := os.ReadFile(d.sql)
		if err != nil {
			return fmt.Errorf("读 %s: %w（在工程根目录运行）", d.sql, err)
		}
		if err := migrate.Run(ctx, db, string(ddl)); err != nil {
			db.Close()
			return fmt.Errorf("建表 %s: %w", d.name, err)
		}
		db.Close()
	}

	log.Println("[seed] 3/10 core")
	coreDB, err := pg.Open("core_db")
	if err != nil {
		return err
	}
	defer coreDB.Close()
	demand, fixed := domains.GenAccountRows(cfg)
	demandNos := make([]string, len(demand))
	for i, d := range demand {
		demandNos[i] = d.AccountNo
	}
	if err := domains.WriteStatic(ctx, coreDB, domains.GenStaticData(cfg)); err != nil {
		return err
	}
	if err := domains.WriteAccounts(ctx, coreDB, demand, fixed); err != nil {
		return err
	}
	if err := domains.RunBizDate(ctx, coreDB, cfg, demandNos); err != nil {
		return err
	}

	log.Println("[seed] 4/10 customer")
	// cust_id/account_no 编号规则与 core 一致 → 确定性关联
	nCustomers := cfg.TargetCounts().DemandAccounts / 2
	if nCustomers < 1 {
		nCustomers = 1
	}
	customers := domains.GenCustomers(cfg, nCustomers)
	custIDs := make([]string, len(customers))
	for i, c := range customers {
		custIDs[i] = c.CustID
	}
	// cust_account_rel：每个 core 活期账户一条户主关系
	pairs := make([][2]string, len(demand))
	for i, d := range demand {
		pairs[i] = [2]string{d.CustID, d.AccountNo}
	}
	rels := domains.GenAccountRels(pairs)
	custDB, err := pg.Open("cust_db")
	if err != nil {
		return err
	}
	if err := domains.WriteCustomers(ctx, custDB, customers); err != nil {
		custDB.Close()
		return err
	}
	if err := domains.WriteAccountRels(ctx, custDB, rels); err != nil {
		custDB.Close()
		return err
	}
	custDB.Close()

	log.Println("[seed] 5/10 payment")
	tc := cfg.TargetCounts()
	nMerchants := 50 // dev 缩影
	if tc.DemandAccounts > 4000 {
		nMerchants = 200 // full
	}
	merchants := domains.GenMerchants(cfg, nMerchants)
	merchantIDs := make([]string, len(merchants))
	for i, m := range merchants {
		merchantIDs[i] = m.MerchantID
	}
	// 缩影量级：转账/消费各 dev 级一批（不做切日滚存）
	nTransfer := tc.DemandAccounts / 2
	nConsumption := tc.DemandAccounts
	transfers := domains.GenTransfers(cfg, demandNos, nTransfer)
	consumptions := domains.GenConsumptions(cfg, demandNos, merchantIDs, nConsumption)
	payDB, err := pg.Open("pay_db")
	if err != nil {
		return err
	}
	if err := domains.WritePayments(ctx, payDB, merchants, transfers, consumptions); err != nil {
		payDB.Close()
		return err
	}
	payDB.Close()

	log.Println("[seed] 6/10 reward")
	rewardStatic := domains.GenRewardStatic(cfg, custIDs)
	campaignIDs := make([]string, len(rewardStatic.Campaigns))
	for i, c := range rewardStatic.Campaigns {
		campaignIDs[i] = c.CampaignID
	}
	rewardDB, err := pg.Open("reward_db")
	if err != nil {
		return err
	}
	if err := domains.WriteRewardStatic(ctx, rewardDB, rewardStatic); err != nil {
		rewardDB.Close()
		return err
	}
	if err := domains.RunReward(ctx, rewardDB, cfg, rewardStatic.PointsAccts, campaignIDs); err != nil {
		rewardDB.Close()
		return err
	}
	rewardDB.Close()

	log.Println("[seed] 7/10 risk")
	riskStatic := domains.GenRiskStatic(cfg, custIDs)
	riskDB, err := pg.Open("risk_db")
	if err != nil {
		return err
	}
	if err := domains.WriteRiskStatic(ctx, riskDB, riskStatic); err != nil {
		riskDB.Close()
		return err
	}
	if err := domains.RunRisk(ctx, riskDB, cfg, custIDs, demandNos); err != nil {
		riskDB.Close()
		return err
	}
	riskDB.Close()

	log.Println("[seed] 8/10 loan")
	loanStatic := domains.GenLoanStatic(cfg, custIDs)
	loanDB, err := pg.Open("loan_db")
	if err != nil {
		return err
	}
	if err := domains.WriteLoanStatic(ctx, loanDB, loanStatic); err != nil {
		loanDB.Close()
		return err
	}
	if err := domains.RunLoan(ctx, loanDB, cfg, loanStatic.Accounts); err != nil {
		loanDB.Close()
		return err
	}
	loanDB.Close()

	log.Println("[seed] 9/10 wealth")
	wealthStatic := domains.GenWealthStatic(cfg, custIDs, demandNos)
	wealthDB, err := pg.Open("wealth_db")
	if err != nil {
		return err
	}
	if err := domains.WriteWealthStatic(ctx, wealthDB, wealthStatic); err != nil {
		wealthDB.Close()
		return err
	}
	if err := domains.RunWealth(ctx, wealthDB, cfg, wealthStatic.Products, wealthStatic.Holdings, custIDs, demandNos); err != nil {
		wealthDB.Close()
		return err
	}
	wealthDB.Close()

	log.Println("[seed] 10/10 setup_fdw")
	if err := fdw.SetupFDW(ctx); err != nil {
		return fmt.Errorf("setup_fdw: %w", err)
	}
	return nil
}

// ensureDBs（同 Task 2，保留不动）。
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
