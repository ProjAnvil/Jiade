// Package fdw 用 postgres_fdw 在各业务库建立对其他库的外部表映射（跨库联邦）。
// 联邦对象是同一 postgres 实例的其他库，故 server host 统一 localhost。
package fdw

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/platform/pg"
)

// Mapping 在 host_db 引入 remote_db 的若干表（外部表名 ext_{remote}_{tbl}）。
type Mapping struct {
	Host   string
	Remote string
	Tables []string
}

// Mappings 覆盖 core/cust/pay/reward/risk 五库联邦（移植 bossy fdw.py + B-1 扩展 cust_db←core_db）。
var Mappings = []Mapping{
	{Host: "core_db", Remote: "cust_db", Tables: []string{"cust_info", "cust_account_rel"}},
	{Host: "cust_db", Remote: "core_db", Tables: []string{"demand_account"}}, // B-1 新增
	{Host: "pay_db", Remote: "core_db", Tables: []string{"demand_account"}},
	{Host: "pay_db", Remote: "cust_db", Tables: []string{"cust_info"}},
	{Host: "reward_db", Remote: "cust_db", Tables: []string{"cust_info"}}, // bossy 已有
	{Host: "risk_db", Remote: "cust_db", Tables: []string{"cust_info"}},   // B-4a 新增（bossy 无）
}

// SetupFDW 在各 host 库幂等建立 extension/server/user_mapping/foreign_table。
// host=localhost（pg 进程连自己实例的其他库），user/pass 取 env（默认 bank/bank）。
func SetupFDW(ctx context.Context) error {
	for _, m := range Mappings {
		db, err := pg.Open(m.Host)
		if err != nil {
			return fmt.Errorf("fdw: 打开 host %s: %w", m.Host, err)
		}
		if err := setupOne(ctx, db, m); err != nil {
			db.Close()
			return fmt.Errorf("fdw: %s ← %s: %w", m.Host, m.Remote, err)
		}
		db.Close()
	}
	return nil
}

func setupOne(ctx context.Context, db *sql.DB, m Mapping) error {
	server := "fdw_" + m.Remote
	stmts := []string{
		"CREATE EXTENSION IF NOT EXISTS postgres_fdw",
		fmt.Sprintf("DROP SERVER IF EXISTS %s CASCADE", server),
		fmt.Sprintf("CREATE SERVER %s FOREIGN DATA WRAPPER postgres_fdw "+
			"OPTIONS (host 'localhost', port '5432', dbname '%s')", server, m.Remote),
		fmt.Sprintf("DROP USER MAPPING IF EXISTS FOR CURRENT_USER SERVER %s", server),
		fmt.Sprintf("CREATE USER MAPPING FOR CURRENT_USER SERVER %s "+
			"OPTIONS (user '%s', password '%s')", server,
			pg.Getenv("DB_USER", "bank"), pg.Getenv("DB_PASSWORD", "bank")),
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("exec %q: %w", s, err)
		}
	}
	for _, tbl := range m.Tables {
		ft := "ext_" + m.Remote + "_" + tbl
		for _, s := range []string{
			fmt.Sprintf("DROP FOREIGN TABLE IF EXISTS %s", ft),
			fmt.Sprintf("DROP FOREIGN TABLE IF EXISTS %s", tbl), // 防御 IMPORT 后未改名残留
			fmt.Sprintf("IMPORT FOREIGN SCHEMA public LIMIT TO (%s) FROM SERVER %s INTO public", tbl, server),
			fmt.Sprintf("ALTER FOREIGN TABLE %s RENAME TO %s", tbl, ft),
		} {
			if _, err := db.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("exec %q: %w", s, err)
			}
		}
	}
	return nil
}
