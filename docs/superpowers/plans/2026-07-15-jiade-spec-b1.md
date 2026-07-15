# Jiade Spec B-1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. After each task, append a line to `.superpowers/sdd/progress.md` per Spec A's ledger convention.

**Goal:** 在 bank 模板里纵切 customer + payment 两个只读服务，建立多库 + postgres_fdw 跨库联邦模式，使 `jiade init → up → seed` 后可 curl 三服务 healthz + 两个跨库 FDW JOIN 端点。

**Architecture:** 单 postgres 实例 3 库（core_db/cust_db/pay_db），每域一个独立 Go 进程（cmd 入口 + `internal/<域>/{domain,repo,service,api}` 四层），跨库查询走 postgres_fdw 外部表（seed 末尾 `setup_fdw` 幂等建立）。fixture 确定性跨域关联（cust_id/account_no 编号规则与 Spec A core 一致），不重构 Spec A core。

**Tech Stack:** Go 1.22 / net/http + chi v5 / database/sql + pgx v5 / postgres_fdw / math/rand/v2 PCG。

## Global Constraints

> 每个任务的隐含前置条件。逐字来自 spec §14 + Spec A 教训（`.superpowers/sdd/progress.md`）。

- **bank module 的 `go.mod` 必须保持 `go 1.22`**：`templates/bank/go.mod` 已 pin 1.22，勿改 toolchain 行；若 `go mod tidy` 改了，用 `go mod edit -go=1.22` 修复。
- **本地验证（macOS 15）一律加 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`** 前缀（Darwin 25 dyld 链接器 bug）。CI（Linux）直接 `go test`。
- **bank 命令在 `templates/bank/` 目录下跑**（独立 module `module bank`）。
- **金额 int64 分，禁 float**：payment 域 Money 与 core `domain.Money` 同构（int64 + 纯整数解析），repo 做分↔NUMERIC 转换。
- **依赖方向向内** `api → service → repo → domain`；各域 `domain` 互不依赖；`repo` 不 import `service`。
- **改完 `templates/bank/` 须重新打包 `templates.tar`**：jiade 仓根 `go generate ./internal/template`（Task 14 负责）。
- **DB 连接**统一 `pg.Open(dbName)`，env `DB_HOST/DB_PORT/DB_USER/DB_PASSWORD`（bank/bank）。
- **FDW server host 统一 `localhost`**（pg 进程视角连自己），port 5432，user/pass bank/bank。
- **确定性**：fixture 用 `fixtures.NewRNG(cfg.Seed+offset)`，各域 offset 独立（core 已用 +1/+2/+3；customer +10/+11；payment +20/+21）。
- **本地 5432 可能被占**：集成/e2e 测试若 5432 冲突，临时 `docker stop bossy-postgres` 或用 `DB_PORT=5433` + 临时 postgres 容器（CI 无此问题）。

---

## Task 1: cust_db + pay_db schema

**Files:**
- Create: `templates/bank/db/migrations/cust_db.sql`
- Create: `templates/bank/db/migrations/pay_db.sql`
- Test: `templates/bank/internal/platform/migrate/migrate_test.go`（修改）

**Interfaces:**
- Consumes: `migrate.SplitStatements`（Spec A 已有）
- Produces: 两个 schema 文件，被 Task 2 建表、Task 3 的 FDW `IMPORT FOREIGN SCHEMA` 引用源表

- [ ] **Step 1: 创建 cust_db.sql**（移植 bossy `schema/cust_db.sql`，5 表，纯 `CREATE TABLE IF NOT EXISTS`，无嵌套分号）

`templates/bank/db/migrations/cust_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS cust_info (
    cust_id        TEXT PRIMARY KEY,
    cust_type      TEXT NOT NULL,
    name           TEXT NOT NULL,
    cert_type      TEXT,
    cert_no        TEXT,
    gender         TEXT,
    birthday       DATE,
    nationality    TEXT,
    risk_level     TEXT,
    kyc_status     TEXT,
    create_biz_date DATE NOT NULL,
    create_ts      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cust_id_doc (
    doc_id      TEXT PRIMARY KEY,
    cust_id     TEXT NOT NULL,
    cert_type   TEXT NOT NULL,
    cert_no     TEXT NOT NULL,
    issue_date  DATE,
    expire_date DATE,
    issue_org   TEXT
);

CREATE TABLE IF NOT EXISTS cust_contact (
    contact_id  TEXT PRIMARY KEY,
    cust_id     TEXT NOT NULL,
    phone       TEXT,
    email       TEXT,
    address     TEXT,
    region_code TEXT,
    is_primary  INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS cust_org (
    cust_id         TEXT PRIMARY KEY,
    org_name        TEXT,
    industry_code   TEXT,
    regist_capital  NUMERIC(18,2),
    legal_rep       TEXT,
    establish_date  DATE
);

CREATE TABLE IF NOT EXISTS cust_account_rel (
    rel_id     TEXT PRIMARY KEY,
    cust_id    TEXT NOT NULL,
    account_no TEXT NOT NULL,
    role       TEXT,
    rel_type   TEXT
);
CREATE INDEX IF NOT EXISTS idx_cust_account_rel_cust ON cust_account_rel(cust_id);
```

- [ ] **Step 2: 创建 pay_db.sql**（移植 bossy `schema/pay_db.sql`，6 表）

`templates/bank/db/migrations/pay_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS transfer_txn (
    txn_id       TEXT PRIMARY KEY,
    biz_date     DATE NOT NULL,
    txn_ts       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    out_account  TEXT NOT NULL,
    in_account   TEXT NOT NULL,
    amount       NUMERIC(18,2) NOT NULL,
    ccy          TEXT NOT NULL,
    fee          NUMERIC(18,2) DEFAULT 0,
    channel      TEXT,
    counter_bank TEXT,
    status       TEXT DEFAULT 'success',
    summary      TEXT
);
CREATE INDEX IF NOT EXISTS idx_transfer_txn_bizdate ON transfer_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_transfer_txn_acct ON transfer_txn(out_account, biz_date);

CREATE TABLE IF NOT EXISTS consumption_txn (
    txn_id      TEXT PRIMARY KEY,
    biz_date    DATE NOT NULL,
    txn_ts      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    account_no  TEXT NOT NULL,
    merchant_id TEXT,
    mcc         TEXT,
    amount      NUMERIC(18,2) NOT NULL,
    ccy         TEXT NOT NULL,
    status      TEXT DEFAULT 'success',
    summary     TEXT
);
CREATE INDEX IF NOT EXISTS idx_consumption_txn_bizdate ON consumption_txn(biz_date);

CREATE TABLE IF NOT EXISTS channel_txn (
    txn_id     TEXT PRIMARY KEY,
    biz_date   DATE NOT NULL,
    txn_ts     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    channel    TEXT NOT NULL,
    device     TEXT,
    cust_id    TEXT,
    status     TEXT DEFAULT 'success',
    latency_ms INTEGER
);

CREATE TABLE IF NOT EXISTS merchant (
    merchant_id     TEXT PRIMARY KEY,
    merchant_name   TEXT NOT NULL,
    mcc             TEXT,
    region          TEXT,
    status          TEXT DEFAULT 'active',
    create_biz_date DATE
);

CREATE TABLE IF NOT EXISTS fee_record (
    fee_id        TEXT PRIMARY KEY,
    biz_date      DATE NOT NULL,
    txn_id        TEXT,
    fee_type      TEXT,
    amount        NUMERIC(18,2) NOT NULL,
    ccy           TEXT NOT NULL,
    pay_or_receive TEXT DEFAULT 'receive'
);

CREATE TABLE IF NOT EXISTS settlement_record (
    settle_id  TEXT PRIMARY KEY,
    biz_date   DATE NOT NULL,
    channel    TEXT,
    net_amount NUMERIC(18,2) NOT NULL,
    txn_count  INTEGER,
    status     TEXT DEFAULT 'settled',
    settle_ts  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

- [ ] **Step 3: 扩展 migrate_test 断言新 schema 可切分**

在 `templates/bank/internal/platform/migrate/migrate_test.go` 末尾追加（若文件已有其他测试，追加新测试函数）:
```go
func TestSplitStatements_CustPaySchemas(t *testing.T) {
	for _, name := range []string{"cust_db.sql", "pay_db.sql"} {
		sql, err := os.ReadFile("../../../../db/migrations/" + name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		stmts := SplitStatements(string(sql))
		if len(stmts) == 0 {
			t.Errorf("%s 切分后无语句", name)
		}
		for _, s := range stmts {
			if !strings.Contains(s, "CREATE") {
				t.Errorf("%s 含非 DDL 语句: %q", name, s)
			}
		}
	}
}
```
（若 `migrate_test.go` 未 import `os`/`strings`，补充 import。）

- [ ] **Step 4: 跑测试**

Run（在 `templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/platform/migrate/...
```
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/db/migrations/cust_db.sql templates/bank/db/migrations/pay_db.sql templates/bank/internal/platform/migrate/migrate_test.go
git commit -m "feat(bank): add cust_db + pay_db schemas (B-1)"
```

---

## Task 2: 多库建库 + 多库建表（seed 泛化）

**Files:**
- Modify: `templates/bank/cmd/seed/main.go`
- Test: `templates/bank/cmd/seed/seed_test.go`（新建，`//go:build integration`）

**Interfaces:**
- Consumes: `pg.Open`、`migrate.Run`、Task 1 的 3 个 schema
- Produces: `ensureDBs(ctx, reset, names)`；seed main 建多库 + 对每库 `migrate.Run`

- [ ] **Step 1: 写集成测试（先失败）**

`templates/bank/cmd/seed/seed_test.go`:
```go
//go:build integration

package main

import (
	"context"
	"testing"

	"bank/internal/platform/pg"
)

func TestEnsureDBs_CreatesAllThree(t *testing.T) {
	ctx := context.Background()
	// 先确保 admin 可连
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres 管理库连接，跳过: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up）: %v", err)
	}
	if err := ensureDBs(ctx, true, []string{"core_db", "cust_db", "pay_db"}); err != nil {
		t.Fatalf("ensureDBs 失败: %v", err)
	}
	for _, name := range []string{"core_db", "cust_db", "pay_db"} {
		var exists bool
		if err := admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("库 %s 未被创建", name)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./cmd/seed/...
```
Expected: FAIL（`ensureDBs` undefined；当前只有 `ensureDB`）。

- [ ] **Step 3: 重构 ensureDB → ensureDBs，并改 seed main 建多库多表**

把 `templates/bank/cmd/seed/main.go` 的 `ensureDB` 替换为 `ensureDBs`，并改写 `main` 的建库建表段。完整新文件:

```go
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
```

> 注意：core 灌数据这段（`GenAccountRows`…`WriteTxns`）是从 Spec A 原 `main` 原样搬入，行为不变。

- [ ] **Step 4: 跑测试确认通过**

Run（`templates/bank/`，需 postgres 起）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./cmd/seed/...
```
Expected: PASS（3 库被创建）。再跑普通测试确认 core 路径未破:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./...
```
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/cmd/seed/main.go templates/bank/cmd/seed/seed_test.go
git commit -m "feat(bank): generalize seed to multi-db (ensureDBs, 3-db migrate)"
```

---

## Task 3: FDW 联邦包（提前，供后续 repo 集成测试）

**Files:**
- Create: `templates/bank/internal/platform/fdw/fdw.go`
- Create: `templates/bank/internal/platform/fdw/fdw_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: `pg.Open`、各库表已建（Task 2）
- Produces: `fdw.Mappings`（含 cust_db←core_db 扩展）、`fdw.SetupFDW(ctx) error`（幂等）

- [ ] **Step 1: 写集成测试（先失败）**

`templates/bank/internal/platform/fdw/fdw_test.go`:
```go
//go:build integration

package fdw_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/fdw"
	"bank/internal/platform/pg"
)

func mustAdmin(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up + make seed 建 3 库表）: %v", err)
	}
	return db
}

func TestSetupFDW_CreatesForeignTables(t *testing.T) {
	ctx := context.Background()
	admin := mustAdmin(t)
	defer admin.Close()
	// 前置：3 库表已存在（Task 2 的 ensureDBs + migrate 应已跑过；此处防御性建表）
	if err := fdw.SetupFDW(ctx); err != nil {
		t.Fatalf("SetupFDW 失败: %v", err)
	}
	// 在 cust_db 应能查到 ext_core_db_demand_account 外部表（来自 core_db.demand_account）
	cust, err := pg.Open("cust_db")
	if err != nil {
		t.Fatal(err)
	}
	defer cust.Close()
	if _, err := cust.ExecContext(ctx, "SELECT account_no FROM ext_core_db_demand_account LIMIT 1"); err != nil {
		t.Errorf("cust_db 查 ext_core_db_demand_account 失败，FDW 未建好: %v", err)
	}
	// 幂等：再跑一次不报错
	if err := fdw.SetupFDW(ctx); err != nil {
		t.Errorf("SetupFDW 二次运行应幂等，失败: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/platform/fdw/...
```
Expected: FAIL（`fdw` 包不存在）。

- [ ] **Step 3: 实现 fdw 包**

`templates/bank/internal/platform/fdw/fdw.go`:
```go
// Package fdw 用 postgres_fdw 在各业务库建立对其他库的外部表映射（跨库联邦）。
// 联邦对象是同一 postgres 实例的其他库，故 server host 统一 localhost。
package fdw

import (
	"context"
	"fmt"
	"database/sql"

	"bank/internal/platform/pg"
)

// Mapping 在 host_db 引入 remote_db 的若干表（外部表名 ext_{remote}_{tbl}）。
type Mapping struct {
	Host   string
	Remote string
	Tables []string
}

// Mappings 覆盖 core/cust/pay 三库联邦（移植 bossy fdw.py + B-1 扩展 cust_db←core_db）。
var Mappings = []Mapping{
	{Host: "core_db", Remote: "cust_db", Tables: []string{"cust_info", "cust_account_rel"}},
	{Host: "cust_db", Remote: "core_db", Tables: []string{"demand_account"}}, // B-1 新增
	{Host: "pay_db", Remote: "core_db", Tables: []string{"demand_account"}},
	{Host: "pay_db", Remote: "cust_db", Tables: []string{"cust_info"}},
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

```

> `pg.Getenv` 不存在——下面在 `pg` 包补一个导出的 `Getenv`（Step 4）。若不愿改 `pg`，可改为在本文件内复制 `os.Getenv` 逻辑；但为 DRY，统一走 `pg.Getenv`。

- [ ] **Step 4: 在 pg 包补导出 Getenv**

修改 `templates/bank/internal/platform/pg/pg.go`：把私有 `getenv` 改名为导出 `Getenv`（并更新同文件内 `DSN` 的调用）。
```go
// Getenv 读环境变量，空则返回 def。
func Getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```
`DSN` 内 4 处 `getenv(...)` 改为 `Getenv(...)`；删除原私有 `getenv`。

- [ ] **Step 5: 跑测试**

Run（`templates/bank/`，需 postgres + 3 库表已建，可先 `make seed`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/platform/fdw/...
```
Expected: PASS（外部表建好 + 幂等）。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/platform/fdw/ templates/bank/internal/platform/pg/pg.go
git commit -m "feat(bank): add postgres_fdw federation setup (multi-db mappings)"
```

---

## Task 4: customer domain

**Files:**
- Create: `templates/bank/internal/customer/domain/customer.go`
- Create: `templates/bank/internal/customer/domain/customer_test.go`

**Interfaces:**
- Produces: `domain.Customer`、`domain.AccountRel`、`domain.CustAccount`、`domain.CustType` 常量

- [ ] **Step 1: 写失败测试**

`templates/bank/internal/customer/domain/customer_test.go`:
```go
package domain

import "testing"

func TestCustomerTypes(t *testing.T) {
	if CustTypePersonal != "个人" || CustTypeOrg != "对公" {
		t.Errorf("客户类型常量错误: %q %q", CustTypePersonal, CustTypeOrg)
	}
	c := Customer{CustID: "C0000001", CustType: CustTypePersonal, Name: "张伟", KYCStatus: "passed"}
	if c.CustID != "C0000001" {
		t.Errorf("cust_id=%s", c.CustID)
	}
}

func TestParseCustAccount(t *testing.T) {
	a := CustAccount{AccountNo: "D0000000001", Ccy: "CNY", Status: "active", Role: "主"}
	if a.AccountNo != "D0000000001" {
		t.Errorf("account_no=%s", a.AccountNo)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/customer/domain/...`
Expected: FAIL（类型未定义）。

- [ ] **Step 3: 实现 domain**

`templates/bank/internal/customer/domain/customer.go`:
```go
// Package domain 是 customer 服务的纯领域模型，零 DB/框架依赖（最内层）。
package domain

// CustType 客户类型。
type CustType string

const (
	CustTypePersonal CustType = "个人"
	CustTypeOrg      CustType = "对公"
)

// Customer 对应 cust_info 表。
type Customer struct {
	CustID        string
	CustType      CustType
	Name          string
	CertType      string
	CertNo        string
	Gender        string // M/F；对公为空
	Birthday      string // YYYY-MM-DD；对公为空
	Nationality   string
	RiskLevel     string // low/medium
	KYCStatus     string // passed
	CreateBizDate string
}

// AccountRel 对应 cust_account_rel 表（客户-账户关系）。
type AccountRel struct {
	RelID     string
	CustID    string
	AccountNo string
	Role      string // 主/共
	RelType   string // 户主
}

// CustAccount 是跨库联邦查询结果（cust_account_rel JOIN ext_core_db_demand_account）。
type CustAccount struct {
	AccountNo   string
	Ccy         string
	Status      string
	OpenBizDate string
	BranchCode  string
	Role        string
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/customer/domain/...`
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/customer/domain/
git commit -m "feat(bank): customer domain model"
```

---

## Task 5: customer fixture 生成器 + rng 词库扩展

**Files:**
- Modify: `templates/bank/internal/fixtures/rng.go`（加词库 + 日期 helper）
- Create: `templates/bank/internal/fixtures/domains/customer.go`
- Create: `templates/bank/internal/fixtures/domains/customer_test.go`

**Interfaces:**
- Consumes: `fixtures.Config`/`RNG`/新词库、`customer/domain`
- Produces: `domains.GenCustomers(cfg, n)`、`domains.GenAccountRels(pairs [][2]string)`、`domains.WriteCustomers(ctx, db, ...)`

- [ ] **Step 1: 写确定性测试（先失败）**

`templates/bank/internal/fixtures/domains/customer_test.go`:
```go
package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenCustomers_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	a := GenCustomers(cfg, 20)
	b := GenCustomers(cfg, 20)
	if !reflect.DeepEqual(a, b) {
		t.Error("GenCustomers 不确定性")
	}
	if len(a) != 20 || a[0].CustID != "C0000001" {
		t.Errorf("首行 cust_id=%s len=%d", a[0].CustID, len(a))
	}
	// 20% 对公：j%5==0 → 第 0 个对公
	if a[0].CustType != "对公" {
		t.Errorf("j=0 应对公，got %s", a[0].CustType)
	}
	if a[1].CustType != "个人" {
		t.Errorf("j=1 应个人，got %s", a[1].CustType)
	}
}

func TestGenAccountRels_LinksCustToAccount(t *testing.T) {
	pairs := [][2]string{{"C0000001", "D0000000001"}, {"C0000001", "D0000000002"}}
	rels := GenAccountRels(pairs)
	if len(rels) != 2 || rels[0].CustID != "C0000001" || rels[0].AccountNo != "D0000000001" {
		t.Errorf("rel 关联错误: %+v", rels)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...`
Expected: FAIL（`GenCustomers` undefined）。

- [ ] **Step 3: 扩展 rng.go 词库 + 日期 helper**

在 `templates/bank/internal/fixtures/rng.go` 的 `var(...)` 块追加（保留现有词库）:
```go
var (
	Surnames   = []string{"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "吴", "周"}
	GivenNames = []string{"伟", "芳", "娜", "秀英", "敏", "静", "磊", "强", "洋", "艳"}
	Branches   = []string{"HO", "SH", "BJ", "GZ", "CD"}
	Channels   = []string{"网银", "手机", "ATM", "柜面"}
	Summaries  = []string{"工资", "转账", "消费", "存款", "取款"}

	// B-1 新增词库
	Genders           = []string{"M", "F"}
	RiskLevels        = []string{"low", "low", "low", "medium"} // 75% low
	CustRegions       = []string{"华东", "华北", "华南", "西南"}
	Industries        = []string{"A", "B", "C", "F", "G", "I", "K"}
	MCCs              = []string{"5411", "5912", "7011", "4111", "5310", "5732", "5812"}
	TransferSummaries = []string{"转账", "汇款", "还款"}
	CounterBanks      = []string{"本行", "他行"}
	Devices           = []string{"PC", "APP", "ATM", "柜台"}
)
```
并在 rng.go 末尾加日期 helper（需 import `time`）:
```go
// RandomDate 返回 [start,end]（YYYY-MM-DD）区间内的一个确定性随机日期。
func RandomDate(g *RNG, start, end string) string {
	t0, err := time.Parse("2006-01-02", start)
	if err != nil {
		return start
	}
	t1, err := time.Parse("2006-01-02", end)
	if err != nil {
		return end
	}
	days := int(t1.Sub(t0).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return t0.AddDate(0, 0, g.IntRange(0, days)).Format("2006-01-02")
}
```
rng.go 顶部 import 块改为 `import ("math/rand/v2"; "time")`。

- [ ] **Step 4: 实现 customer fixture 生成器**

`templates/bank/internal/fixtures/domains/customer.go`:
```go
package domains

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/customer/domain"
	"bank/internal/fixtures"
)

// GenCustomers 生成 n 个客户（cust_id=C%07d(j+1)，与 core 账户的 cust_id 编号一致）。
// 20% 对公（j%5==0）。rng 偏移 +10。
func GenCustomers(cfg fixtures.Config, n int) []domain.Customer {
	rng := fixtures.NewRNG(cfg.Seed + 10)
	out := make([]domain.Customer, n)
	for j := 0; j < n; j++ {
		isOrg := j%5 == 0
		c := domain.Customer{
			CustID:        fmt.Sprintf("C%07d", j+1),
			Nationality:   "CN",
			RiskLevel:     rng.Choice(fixtures.RiskLevels),
			KYCStatus:     "passed",
			CreateBizDate: fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
		}
		if isOrg {
			c.CustType = domain.CustTypeOrg
			c.Name = orgName(rng)
			c.CertType = "统一社会信用代码"
			c.CertNo = numerify(rng, 18)
		} else {
			c.CustType = domain.CustTypePersonal
			c.Name = rng.Choice(fixtures.Surnames) + rng.Choice(fixtures.GivenNames)
			c.CertType = "身份证"
			c.CertNo = numerify(rng, 18)
			c.Gender = rng.Choice(fixtures.Genders)
			c.Birthday = fixtures.RandomDate(rng, "1950-01-01", "2007-12-31")
		}
		out[j] = c
	}
	return out
}

// GenAccountRels 由 (custID, accountNo) 对生成户主关系。rel_id 确定性。
func GenAccountRels(pairs [][2]string) []domain.AccountRel {
	out := make([]domain.AccountRel, len(pairs))
	for i, p := range pairs {
		out[i] = domain.AccountRel{
			RelID: fmt.Sprintf("R%010d", i+1), CustID: p[0], AccountNo: p[1],
			Role: "主", RelType: "户主",
		}
	}
	return out
}

// WriteCustomers 幂等写 cust_info（先 DELETE 后 INSERT）。
func WriteCustomers(ctx context.Context, db *sql.DB, rows []domain.Customer) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM cust_info"); err != nil {
		return fmt.Errorf("清空 cust_info: %w", err)
	}
	for _, c := range rows {
		var gender, birthday any
		if c.Gender != "" {
			gender = c.Gender
		}
		if c.Birthday != "" {
			birthday = c.Birthday
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO cust_info
			(cust_id,cust_type,name,cert_type,cert_no,gender,birthday,nationality,risk_level,kyc_status,create_biz_date)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			c.CustID, string(c.CustType), c.Name, c.CertType, c.CertNo,
			gender, birthday, c.Nationality, c.RiskLevel, c.KYCStatus, c.CreateBizDate); err != nil {
			return fmt.Errorf("插入 cust_info %s: %w", c.CustID, err)
		}
	}
	return nil
}

// WriteAccountRels 幂等写 cust_account_rel。
func WriteAccountRels(ctx context.Context, db *sql.DB, rels []domain.AccountRel) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM cust_account_rel"); err != nil {
		return err
	}
	for _, r := range rels {
		if _, err := db.ExecContext(ctx, `INSERT INTO cust_account_rel
			(rel_id,cust_id,account_no,role,rel_type) VALUES ($1,$2,$3,$4,$5)`,
			r.RelID, r.CustID, r.AccountNo, r.Role, r.RelType); err != nil {
			return err
		}
	}
	return nil
}

// orgName 生成对公客户名（行业 + "有限公司"）。
func orgName(rng *fixtures.RNG) string {
	return rng.Choice(fixtures.Industries) + rng.Choice(fixtures.CustRegions) + "有限公司"
}

// numerify 生成 n 位数字串（确定性）。
func numerify(rng *fixtures.RNG, n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[rng.IntRange(0, 9)]
	}
	return string(b)
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...`
Expected: PASS（确定性 + 首行 cust_id + 对公/个人分布）。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/fixtures/rng.go templates/bank/internal/fixtures/domains/customer.go templates/bank/internal/fixtures/domains/customer_test.go
git commit -m "feat(bank): customer fixture generator + rng wordbank/date helpers"
```

---

## Task 6: customer repo（本库查询 + FDW JOIN）

**Files:**
- Create: `templates/bank/internal/customer/repo/customer_repo.go`
- Create: `templates/bank/internal/customer/repo/customer_repo_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: `customer/domain`、`pg.Open("cust_db")`、Task 3 的 `ext_core_db_demand_account` 外部表
- Produces: `repo.CustomerRepo`（`GetCustomer`/`ListCustomers`/`GetCustAccounts`）

- [ ] **Step 1: 写集成测试（先失败）**

`templates/bank/internal/customer/repo/customer_repo_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/customer/domain"
	"bank/internal/customer/repo"
	"bank/internal/platform/pg"
)

func setupCustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("cust_db")
	if err != nil {
		t.Skipf("无 cust_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestCustomerRepo_GetAndList(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db)

	db.ExecContext(ctx, "DELETE FROM cust_info WHERE cust_id='IT-C1'")
	db.ExecContext(ctx, `INSERT INTO cust_info(cust_id,cust_type,name,cert_type,cert_no,nationality,risk_level,kyc_status,create_biz_date)
		VALUES ('IT-C1','个人','测试','身份证','110101000000000001','CN','low','passed','2026-01-01')`)

	got, err := r.GetCustomer(ctx, "IT-C1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "测试" || got.CustType != domain.CustTypePersonal {
		t.Errorf("got %+v", got)
	}
	list, err := r.ListCustomers(ctx, "个人", "passed", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Error("ListCustomers 应至少返回 IT-C1")
	}
}

func TestCustomerRepo_GetCustAccounts_FDWJoin(t *testing.T) {
	db := setupCustDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewCustomerRepo(db)
	// 依赖 seed 已建 ext_core_db_demand_account 外部表 + cust_account_rel + core demand_account 数据
	// 取 seed 出的第一个客户的账户（若无数据则跳过）
	accts, err := r.GetCustAccounts(ctx, "C0000001")
	if err != nil {
		t.Fatalf("FDW JOIN 查询失败（外部表未建？先 make seed + setup_fdw）: %v", err)
	}
	// 不强断言行数（取决于 seed 数据是否已灌），只断言查询不报错
	_ = accts
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/customer/repo/...`
Expected: FAIL（`repo.NewCustomerRepo` undefined）。

- [ ] **Step 3: 实现 repo**

`templates/bank/internal/customer/repo/customer_repo.go`:
```go
// Package repo 是 customer 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/customer/domain"
)

type CustomerRepo struct{ db *sql.DB }

func NewCustomerRepo(db *sql.DB) *CustomerRepo { return &CustomerRepo{db: db} }

// GetCustomer 查单个客户。不存在返回包装的 sql.ErrNoRows。
func (r *CustomerRepo) GetCustomer(ctx context.Context, custID string) (domain.Customer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT cust_id,cust_type,name,cert_type,cert_no,gender,birthday,
		nationality,risk_level,kyc_status,create_biz_date FROM cust_info WHERE cust_id=$1`, custID)
	var c domain.Customer
	var cType, gender, birthday sql.NullString
	err := row.Scan(&c.CustID, &cType, &c.Name, &c.CertType, &c.CertNo, &gender, &birthday,
		&c.Nationality, &c.RiskLevel, &c.KYCStatus, &c.CreateBizDate)
	if err != nil {
		return domain.Customer{}, fmt.Errorf("repo: 查客户 %s: %w", custID, err)
	}
	c.CustType = domain.CustType(cType.String)
	c.Gender, c.Birthday = gender.String, birthday.String
	return c, nil
}

// ListCustomers 按客户类型/kyc 筛选（空则不限），分页。
func (r *CustomerRepo) ListCustomers(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT cust_id,cust_type,name,cert_type,cert_no,gender,birthday,nationality,risk_level,kyc_status,create_biz_date
		FROM cust_info WHERE ($1='' OR cust_type=$1) AND ($2='' OR kyc_status=$2)
		ORDER BY cust_id LIMIT $3 OFFSET $4`
	rows, err := r.db.QueryContext(ctx, q, custType, kycStatus, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列客户: %w", err)
	}
	defer rows.Close()
	var out []domain.Customer
	for rows.Next() {
		var c domain.Customer
		var cType, gender, birthday sql.NullString
		if err := rows.Scan(&c.CustID, &cType, &c.Name, &c.CertType, &c.CertNo, &gender, &birthday,
			&c.Nationality, &c.RiskLevel, &c.KYCStatus, &c.CreateBizDate); err != nil {
			return nil, err
		}
		c.CustType = domain.CustType(cType.String)
		c.Gender, c.Birthday = gender.String, birthday.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCustAccounts 跨库联邦查询：cust_account_rel JOIN ext_core_db_demand_account（FDW）。
func (r *CustomerRepo) GetCustAccounts(ctx context.Context, custID string) ([]domain.CustAccount, error) {
	q := `SELECT a.account_no, a.ccy, a.acct_status, a.open_biz_date, a.branch_code, rel.role
		FROM cust_account_rel rel
		JOIN ext_core_db_demand_account a ON rel.account_no = a.account_no
		WHERE rel.cust_id=$1 ORDER BY a.account_no`
	rows, err := r.db.QueryContext(ctx, q, custID)
	if err != nil {
		return nil, fmt.Errorf("repo: 联邦查客户账户 %s: %w", custID, err)
	}
	defer rows.Close()
	var out []domain.CustAccount
	for rows.Next() {
		var a domain.CustAccount
		var branch sql.NullString
		if err := rows.Scan(&a.AccountNo, &a.Ccy, &a.Status, &a.OpenBizDate, &branch, &a.Role); err != nil {
			return nil, err
		}
		a.BranchCode = branch.String
		out = append(out, a)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: 跑测试**

Run（需 postgres + `make seed` 已跑，含 setup_fdw）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/customer/repo/...
```
Expected: PASS（GetAndList 通过；GetCustAccounts 不报错）。若 FDW JOIN 测试因 seed 未灌 customer 数据而无行，测试仍 PASS（只断言不报错）。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/customer/repo/
git commit -m "feat(bank): customer repo with FDW cross-db join"
```

---

## Task 7: customer service + api + cmd

**Files:**
- Create: `templates/bank/internal/customer/service/customer_service.go`
- Create: `templates/bank/internal/customer/api/handlers.go`
- Create: `templates/bank/internal/customer/api/router.go`
- Create: `templates/bank/internal/customer/api/handlers_test.go`
- Create: `templates/bank/cmd/customer/main.go`

**Interfaces:**
- Consumes: Task 6 的 `repo.CustomerRepo`
- Produces: `customer` 服务进程（:8081）

- [ ] **Step 1: 写 handler 单测（先失败）**

`templates/bank/internal/customer/api/handlers_test.go`:
```go
package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/customer/domain"
)

type fakeCustRepo struct {
	c       *domain.Customer
	accts   []domain.CustAccount
	getErr  error
}

func (f fakeCustRepo) GetCustomer(context.Context, string) (domain.Customer, error) {
	if f.getErr != nil {
		return domain.Customer{}, f.getErr
	}
	if f.c != nil {
		return *f.c, nil
	}
	return domain.Customer{}, sql.ErrNoRows
}
func (f fakeCustRepo) ListCustomers(context.Context, string, string, int, int) ([]domain.Customer, error) {
	return nil, nil
}
func (f fakeCustRepo) GetCustAccounts(context.Context, string) ([]domain.CustAccount, error) {
	return f.accts, nil
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetCustomer_OK(t *testing.T) {
	h := &Handlers{Repo: fakeCustRepo{c: &domain.Customer{CustID: "C0000001", Name: "张伟", CustType: domain.CustTypePersonal}}}
	code, body := get(t, NewRouter(h), "/api/v1/customers/C0000001")
	if code != 200 || !strings.Contains(body, `"name":"张伟"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetCustomer_NotFound(t *testing.T) {
	h := &Handlers{Repo: fakeCustRepo{}} // 返回 ErrNoRows
	code, _ := get(t, NewRouter(h), "/api/v1/customers/NOPE")
	if code != 404 {
		t.Errorf("want 404 got %d", code)
	}
}

func TestGetCustAccounts(t *testing.T) {
	h := &Handlers{Repo: fakeCustRepo{accts: []domain.CustAccount{{AccountNo: "D1", Ccy: "CNY", Status: "active", Role: "主"}}}}
	code, body := get(t, NewRouter(h), "/api/v1/customers/C0000001/accounts")
	if code != 200 || !strings.Contains(body, `"account_no":"D1"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/customer/api/...`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现 service**

`templates/bank/internal/customer/service/customer_service.go`:
```go
// Package service 是 customer 服务的用例层（查询编排，纯逻辑可单测）。
package service

import (
	"context"

	"bank/internal/customer/domain"
)

// CustomerStore 客户查询接口（repo 实现）。
type CustomerStore interface {
	GetCustomer(ctx context.Context, custID string) (domain.Customer, error)
	ListCustomers(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error)
	GetCustAccounts(ctx context.Context, custID string) ([]domain.CustAccount, error)
}

type CustomerService struct{ store CustomerStore }

func NewCustomerService(store CustomerStore) *CustomerService { return &CustomerService{store: store} }

func (s *CustomerService) Get(ctx context.Context, custID string) (domain.Customer, error) {
	return s.store.GetCustomer(ctx, custID)
}
func (s *CustomerService) List(ctx context.Context, custType, kycStatus string, offset, limit int) ([]domain.Customer, error) {
	return s.store.ListCustomers(ctx, custType, kycStatus, offset, limit)
}
func (s *CustomerService) Accounts(ctx context.Context, custID string) ([]domain.CustAccount, error) {
	return s.store.GetCustAccounts(ctx, custID)
}
```

- [ ] **Step 4: 实现 api（handlers + router + DTO）**

`templates/bank/internal/customer/api/handlers.go`:
```go
// Package api 是 customer 服务的传输层：http handlers + chi router。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/customer/domain"
	"bank/internal/customer/service"

	"github.com/go-chi/chi/v5"
)

type Handlers struct {
	Svc *service.CustomerService
	// Repo 兼容 handler 单测直接注入（生产由 Svc 代理）；二选一非空即可。
	Repo customerReader
}

// customerReader 让 handler 单测用 fake 注入（与 Svc 同接口形状）。
type customerReader interface {
	GetCustomer(r *http.Request) // 占位避免未用；实际查询走 Svc
}
```
> 上面 `customerReader` 占位会编译错——改为干净设计：handler 直接持有 `*service.CustomerService`，单测用 `service.NewCustomerService(fakeStore)` 注入。重写 handlers.go 如下（用这版，删占位）:

`templates/bank/internal/customer/api/handlers.go`（最终版）:
```go
// Package api 是 customer 服务的传输层：http handlers + chi router。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"database/sql"

	"bank/internal/customer/service"

	"github.com/go-chi/chi/v5"
)

type Handlers struct {
	Svc *service.CustomerService
}

func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handlers) GetCustomer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	c, err := h.Svc.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("客户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, customerResp{
		CustID: c.CustID, CustType: string(c.CustType), Name: c.Name,
		CertType: c.CertType, Gender: c.Gender, Birthday: c.Birthday,
		Nationality: c.Nationality, RiskLevel: c.RiskLevel, KYCStatus: c.KYCStatus,
		CreateBizDate: c.CreateBizDate,
	})
}

func (h *Handlers) ListCustomers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.List(r.Context(), q.Get("type"), q.Get("kyc_status"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]customerResp, 0, len(list))
	for _, c := range list {
		out = append(out, customerResp{
			CustID: c.CustID, CustType: string(c.CustType), Name: c.Name,
			KYCStatus: c.KYCStatus, RiskLevel: c.RiskLevel, CreateBizDate: c.CreateBizDate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"customers": out})
}

func (h *Handlers) GetCustAccounts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	accts, err := h.Svc.Accounts(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]accountResp, 0, len(accts))
	for _, a := range accts {
		out = append(out, accountResp{
			AccountNo: a.AccountNo, Ccy: a.Ccy, Status: a.Status,
			OpenBizDate: a.OpenBizDate, Branch: a.BranchCode, Role: a.Role,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

type customerResp struct {
	CustID       string `json:"cust_id"`
	CustType     string `json:"cust_type"`
	Name         string `json:"name"`
	CertType     string `json:"cert_type,omitempty"`
	Gender       string `json:"gender,omitempty"`
	Birthday     string `json:"birthday,omitempty"`
	Nationality  string `json:"nationality,omitempty"`
	RiskLevel    string `json:"risk_level,omitempty"`
	KYCStatus    string `json:"kyc_status,omitempty"`
	CreateBizDate string `json:"create_biz_date,omitempty"`
}

type accountResp struct {
	AccountNo   string `json:"account_no"`
	Ccy         string `json:"ccy"`
	Status      string `json:"status"`
	OpenBizDate string `json:"open_biz_date"`
	Branch      string `json:"branch_code,omitempty"`
	Role        string `json:"role,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
```

> 单测里的 `Handlers{Repo: fakeCustRepo{...}}` 需改为 `Handlers{Svc: service.NewCustomerService(fakeCustRepo{...})}`。**修正 handlers_test.go 的注入**：把 4 处 `Repo:` 改为 `Svc: service.NewCustomerService(...)`。即 Step 1 测试里的 `h := &Handlers{Repo: fakeCustRepo{...}}` 全部替换为 `h := &Handlers{Svc: service.NewCustomerService(fakeCustRepo{...})}`（需 import `bank/internal/customer/service`）。

`templates/bank/internal/customer/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/customers", h.ListCustomers)
		r.Get("/customers/{cust_id}", h.GetCustomer)
		r.Get("/customers/{cust_id}/accounts", h.GetCustAccounts)
	})
	return r
}
```

- [ ] **Step 5: 实现 cmd/customer/main.go**

`templates/bank/cmd/customer/main.go`（仿 core-banking/main.go，库名 cust_db、repo/service 装配）:
```go
// Package main 是 customer 只读 API 服务入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank/internal/customer/api"
	"bank/internal/customer/repo"
	"bank/internal/customer/service"
	"bank/internal/platform/pg"
)

func main() {
	dbName := getenv("DB_NAME", "cust_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}

	handlers := &api.Handlers{
		Svc: service.NewCustomerService(repo.NewCustomerRepo(db)),
	}
	port := getenv("API_PORT", "8081")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}

	go func() {
		log.Printf("customer 监听 :%s (db=%s)", port, dbName)
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
```

- [ ] **Step 6: 跑测试 + build**

Run:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/customer/...
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./cmd/customer
```
Expected: 测试 PASS；build 成功。

- [ ] **Step 7: Commit**
```bash
git add templates/bank/internal/customer/service/ templates/bank/internal/customer/api/ templates/bank/cmd/customer/
git commit -m "feat(bank): customer service + api + cmd (read-only, :8081)"
```

---

## Task 8: payment domain（Money int64 分 + 模型）

**Files:**
- Create: `templates/bank/internal/payment/domain/money.go`
- Create: `templates/bank/internal/payment/domain/money_test.go`
- Create: `templates/bank/internal/payment/domain/payment.go`

**Interfaces:**
- Produces: `domain.Money`（与 core 同构，禁 float）、`Transfer`/`Consumption`/`Merchant`/`ChannelTxn`/`FeeRecord`/`Settlement`

- [ ] **Step 1: 写 Money 禁 float 测试（先失败）**

`templates/bank/internal/payment/domain/money_test.go`:
```go
package domain

import (
	"os"
	"strings"
	"testing"
)

func TestMoneyRoundTrip(t *testing.T) {
	m := NewMoneyFromCents(123456)
	if m.String() != "1234.56" {
		t.Errorf("String=%s want 1234.56", m.String())
	}
	p, err := ParseCents("1234.56")
	if err != nil || p != m {
		t.Errorf("ParseCents 回环失败: %v %v", p, err)
	}
}

// TestSourceHasNoFloat 守卫 money.go 源码不含 float（金融禁 float）。
func TestSourceHasNoFloat(t *testing.T) {
	b, err := os.ReadFile("money.go")
	if err != nil {
		t.Fatal("读不到 money.go:", err)
	}
	if strings.Contains(string(b), "float") {
		t.Fatal("money.go 含 float，违反金融不变量")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/payment/domain/...`
Expected: FAIL（类型未定义）。

- [ ] **Step 3: 实现 money.go**（从 core `domain/money.go` 复制，package 改 `domain`，去掉 `osReadFile` 该测试已自包含）

`templates/bank/internal/payment/domain/money.go`:
```go
package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// Money 用 int64 分表示金额。金融禁 float。构造仅经 NewMoneyFromCents 或 ParseCents。
type Money int64

func NewMoneyFromCents(cents int64) Money { return Money(cents) }

// ParseCents 把 NUMERIC(18,2) 字符串解析为分。纯整数运算，杜绝 float。
func ParseCents(s string) (Money, error) {
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("money: 小数位超过 2: %q", s)
	}
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	n, err := strconv.ParseInt(intPart+fracPart, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: 解析 %q 失败: %w", s, err)
	}
	if neg {
		n = -n
	}
	return Money(n), nil
}

func (m Money) Add(o Money) Money { return m + o }
func (m Money) Sub(o Money) Money { return m - o }
func (m Money) Cents() int64      { return int64(m) }

// String 返回 NUMERIC(18,2) 风格字符串（写 DB 用）。
func (m Money) String() string {
	n := int64(m)
	neg := n < 0
	if neg {
		n = -n
	}
	yuan, cents := n/100, n%100
	if neg {
		return fmt.Sprintf("-%d.%02d", yuan, cents)
	}
	return fmt.Sprintf("%d.%02d", yuan, cents)
}
```

- [ ] **Step 4: 实现 payment.go 模型**

`templates/bank/internal/payment/domain/payment.go`:
```go
package domain

// Transfer 对应 transfer_txn 表。
type Transfer struct {
	TxnID       string
	BizDate     string
	OutAccount  string
	InAccount   string
	Amount      Money
	Ccy         string
	Fee         Money
	Channel     string
	CounterBank string
	Status      string
	Summary     string
}

// Consumption 对应 consumption_txn 表。
type Consumption struct {
	TxnID      string
	BizDate    string
	AccountNo  string
	MerchantID string
	MCC        string
	Amount     Money
	Ccy        string
	Status     string
	Summary    string
}

// Merchant 对应 merchant 表。
type Merchant struct {
	MerchantID   string
	MerchantName string
	MCC          string
	Region       string
	Status       string
	CreateBizDate string
}

// ChannelTxn 对应 channel_txn 表。
type ChannelTxn struct {
	TxnID     string
	BizDate   string
	Channel   string
	Device    string
	CustID    string
	Status    string
	LatencyMs int
}

// FeeRecord 对应 fee_record 表。
type FeeRecord struct {
	FeeID         string
	BizDate       string
	TxnID         string
	FeeType       string
	Amount        Money
	Ccy           string
	PayOrReceive  string
}

// Settlement 对应 settlement_record 表。
type Settlement struct {
	SettleID  string
	BizDate   string
	Channel   string
	NetAmount Money
	TxnCount  int
	Status    string
}

// TransferParty 是 transfer_txn 联邦 JOIN 结果（账户 + 户主客户姓名）。
type TransferParty struct {
	TxnID       string
	Amount      Money
	Ccy         string
	OutAccount  string
	OutCustName string
	InAccount   string
	InCustName  string
	BizDate     string
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/payment/domain/...`
Expected: PASS（含 float 守卫）。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/payment/domain/
git commit -m "feat(bank): payment domain (int64 Money + txn/merchant models)"
```

---

## Task 9: payment fixture 生成器

**Files:**
- Create: `templates/bank/internal/fixtures/domains/payment.go`
- Create: `templates/bank/internal/fixtures/domains/payment_test.go`

**Interfaces:**
- Consumes: `fixtures.Config`/`RNG`、`payment/domain`、core 的 demandNos
- Produces: `GenMerchants`/`GenTransfers`/`GenConsumptions`/`GenChannelTxns`/`GenFeeRecords` + `WritePayments`（幂等）

- [ ] **Step 1: 写确定性测试（先失败）**

`templates/bank/internal/fixtures/domains/payment_test.go`:
```go
package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenMerchants_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	a := GenMerchants(cfg, 10)
	b := GenMerchants(cfg, 10)
	if !reflect.DeepEqual(a, b) || len(a) != 10 {
		t.Error("GenMerchants 不确定或数量错")
	}
	if a[0].MerchantID != "M00000" {
		t.Errorf("首商户 id=%s", a[0].MerchantID)
	}
}

func TestGenTransfers_UsesCoreAccounts(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	accts := []string{"D0000000001", "D0000000002"}
	ts := GenTransfers(cfg, accts, 5)
	if len(ts) != 5 {
		t.Fatalf("len=%d", len(ts))
	}
	if ts[0].OutAccount != "D0000000001" && ts[0].OutAccount != "D0000000002" {
		t.Errorf("out_account 不在 core 账户集: %s", ts[0].OutAccount)
	}
	// 金额 int64 分（整数 * 100）
	if ts[0].Amount.Cents()%100 != 0 && ts[0].Amount.Cents() < 0 {
		t.Errorf("amount 异常: %d", ts[0].Amount.Cents())
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...`
Expected: FAIL（`GenMerchants` undefined）。

- [ ] **Step 3: 实现 payment fixture**

`templates/bank/internal/fixtures/domains/payment.go`:
```go
package domains

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/fixtures"
	"bank/internal/payment/domain"
)

// GenMerchants 生成 n 个商户（M%05d）。rng 偏移 +20。
func GenMerchants(cfg fixtures.Config, n int) []domain.Merchant {
	rng := fixtures.NewRNG(cfg.Seed + 20)
	out := make([]domain.Merchant, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Merchant{
			MerchantID: fmt.Sprintf("M%05d", i),
			MerchantName: rng.Choice(fixtures.Industries) + rng.Choice(fixtures.CustRegions) + "商戶",
			MCC:          rng.Choice(fixtures.MCCs),
			Region:       rng.Choice(fixtures.CustRegions),
			Status:       "active",
			CreateBizDate: fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
		}
	}
	return out
}

// GenTransfers 用 core 的活期账户生成转账。金额 int64 分（[1,999]元 → cents）。
// rng 偏移 +21。B-1 不做切日滚存，biz_date 散布在范围内（快照式）。
func GenTransfers(cfg fixtures.Config, demandNos []string, n int) []domain.Transfer {
	if len(demandNos) == 0 {
		return nil
	}
	rng := fixtures.NewRNG(cfg.Seed + 21)
	out := make([]domain.Transfer, n)
	for i := 0; i < n; i++ {
		amt := domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 1000) // [10.00, 9990.00]
		out[i] = domain.Transfer{
			TxnID:       fmt.Sprintf("PT%012d", i+1),
			BizDate:     fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
			OutAccount:  rng.Choice(demandNos),
			InAccount:   rng.Choice(demandNos),
			Amount:      amt,
			Ccy:         "CNY",
			Fee:         domain.NewMoneyFromCents(amt.Cents() / 1000), // 0.1% 手续费
			Channel:     rng.Choice(fixtures.Channels),
			CounterBank: rng.Choice(fixtures.CounterBanks),
			Status:      "success",
			Summary:     rng.Choice(fixtures.TransferSummaries),
		}
	}
	return out
}

// GenConsumptions 用 core 账户 + 商户生成消费。rng 偏移 +22。
func GenConsumptions(cfg fixtures.Config, demandNos []string, merchantIDs []string, n int) []domain.Consumption {
	if len(demandNos) == 0 {
		return nil
	}
	if len(merchantIDs) == 0 {
		merchantIDs = []string{"M00000"}
	}
	rng := fixtures.NewRNG(cfg.Seed + 22)
	out := make([]domain.Consumption, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Consumption{
			TxnID:      fmt.Sprintf("PC%012d", i+1),
			BizDate:    fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
			AccountNo:  rng.Choice(demandNos),
			MerchantID: rng.Choice(merchantIDs),
			MCC:        rng.Choice(fixtures.MCCs),
			Amount:     domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 500), // [5.00, 4995.00]
			Ccy:        "CNY", Status: "success", Summary: "消费",
		}
	}
	return out
}

// WritePayments 幂等写 merchant + transfer_txn + consumption_txn（先 DELETE 后 INSERT）。
func WritePayments(ctx context.Context, db *sql.DB,
	merchants []domain.Merchant, transfers []domain.Transfer, consumptions []domain.Consumption) error {
	for _, t := range []string{"consumption_txn", "transfer_txn", "merchant"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, m := range merchants {
		if _, err := db.ExecContext(ctx, `INSERT INTO merchant(merchant_id,merchant_name,mcc,region,status,create_biz_date)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			m.MerchantID, m.MerchantName, m.MCC, m.Region, m.Status, m.CreateBizDate); err != nil {
			return err
		}
	}
	for _, t := range transfers {
		if _, err := db.ExecContext(ctx, `INSERT INTO transfer_txn
			(txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			t.TxnID, t.BizDate, t.OutAccount, t.InAccount, t.Amount.String(), t.Ccy, t.Fee.String(),
			t.Channel, t.CounterBank, t.Status, t.Summary); err != nil {
			return err
		}
	}
	for _, c := range consumptions {
		if _, err := db.ExecContext(ctx, `INSERT INTO consumption_txn
			(txn_id,biz_date,account_no,merchant_id,mcc,amount,ccy,status,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			c.TxnID, c.BizDate, c.AccountNo, nullable(c.MerchantID), c.MCC, c.Amount.String(),
			c.Ccy, c.Status, c.Summary); err != nil {
			return err
		}
	}
	return nil
}
```

> `nullable` 已在 core.go 定义（同包），直接复用。

- [ ] **Step 4: 跑测试确认通过**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/fixtures/...`
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/fixtures/domains/payment.go templates/bank/internal/fixtures/domains/payment_test.go
git commit -m "feat(bank): payment fixture generator (merchant/transfer/consumption)"
```

---

## Task 10: payment repo（本库 + FDW JOIN）

**Files:**
- Create: `templates/bank/internal/payment/repo/payment_repo.go`
- Create: `templates/bank/internal/payment/repo/payment_repo_test.go`（`//go:build integration`）

**Interfaces:**
- Consumes: `payment/domain`、Task 3 的 `ext_core_db_demand_account` + `ext_cust_db_cust_info`
- Produces: `repo.PaymentRepo`（`ListTransfers`/`GetTransfer`/`GetTransferParties` FDW JOIN/`GetMerchant`）

- [ ] **Step 1: 写集成测试（先失败）**

`templates/bank/internal/payment/repo/payment_repo_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/payment/domain"
	"bank/internal/payment/repo"
	"bank/internal/platform/pg"
)

func setupPayDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("pay_db")
	if err != nil {
		t.Skipf("无 pay_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestPaymentRepo_GetMerchant(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewPaymentRepo(db)
	db.ExecContext(ctx, "DELETE FROM merchant WHERE merchant_id='IT-M1'")
	db.ExecContext(ctx, `INSERT INTO merchant(merchant_id,merchant_name,mcc,region,status,create_biz_date)
		VALUES ('IT-M1','测试商户','5411','华东','active','2026-01-01')`)
	m, err := r.GetMerchant(ctx, "IT-M1")
	if err != nil {
		t.Fatal(err)
	}
	if m.MerchantName != "测试商户" {
		t.Errorf("got %+v", m)
	}
}

func TestPaymentRepo_TransfersAndParties(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewPaymentRepo(db)
	_, err := r.ListTransfers(ctx, "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListTransfers 失败: %v", err)
	}
	// 联邦 JOIN 不报错即可（依赖 seed 数据 + setup_fdw）
	_, err = r.GetTransferParties(ctx, "PT000000000001")
	if err != nil {
		t.Errorf("GetTransferParties FDW JOIN 失败（外部表未建？）: %v", err)
	}
}

func TestPaymentRepo_GetTransfer_NotFound(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	_, err := repo.NewPaymentRepo(db).GetTransfer(context.Background(), "NOPE")
	if err == nil {
		t.Error("应返回错误（不存在）")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/payment/repo/...`
Expected: FAIL（`repo.NewPaymentRepo` undefined）。

- [ ] **Step 3: 实现 repo**

`templates/bank/internal/payment/repo/payment_repo.go`:
```go
// Package repo 是 payment 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/payment/domain"
)

type PaymentRepo struct{ db *sql.DB }

func NewPaymentRepo(db *sql.DB) *PaymentRepo { return &PaymentRepo{db: db} }

// ListTransfers 按账户/日期筛选转账（空则不限），分页。
func (r *PaymentRepo) ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary
		FROM transfer_txn WHERE ($1='' OR out_account=$1 OR in_account=$1)
		AND ($2='' OR biz_date>=$2) AND ($3='' OR biz_date<=$3)
		ORDER BY biz_date DESC, txn_id LIMIT $4 OFFSET $5`
	rows, err := r.db.QueryContext(ctx, q, accountNo, from, to, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列转账: %w", err)
	}
	defer rows.Close()
	return scanTransfers(rows)
}

// GetTransfer 查单笔转账。
func (r *PaymentRepo) GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error) {
	row := r.db.QueryRowContext(ctx, `SELECT txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary
		FROM transfer_txn WHERE txn_id=$1`, txnID)
	t, err := scanTransfer(row.Scan)
	if err != nil {
		return domain.Transfer{}, fmt.Errorf("repo: 查转账 %s: %w", txnID, err)
	}
	return t, nil
}

// GetTransferParties 跨库联邦：transfer_txn JOIN ext_core_db_demand_account(×2) JOIN ext_cust_db_cust_info(×2)。
// 返回转账双方账户 + 户主客户姓名。
func (r *PaymentRepo) GetTransferParties(ctx context.Context, txnID string) (domain.TransferParty, error) {
	q := `SELECT t.txn_id, t.amount, t.ccy, t.biz_date,
			t.out_account, oc.name, t.in_account, ic.name
		FROM transfer_txn t
		LEFT JOIN ext_core_db_demand_account od ON t.out_account=od.account_no
		LEFT JOIN ext_cust_db_cust_info oc ON od.cust_id=oc.cust_id
		LEFT JOIN ext_core_db_demand_account id ON t.in_account=id.account_no
		LEFT JOIN ext_cust_db_cust_info ic ON id.cust_id=ic.cust_id
		WHERE t.txn_id=$1`
	var p domain.TransferParty
	var outName, inName sql.NullString
	var amtStr string
	err := r.db.QueryRowContext(ctx, q, txnID).Scan(
		&p.TxnID, &amtStr, &p.Ccy, &p.BizDate,
		&p.OutAccount, &outName, &p.InAccount, &inName)
	if err != nil {
		return domain.TransferParty{}, fmt.Errorf("repo: 联邦查转账双方 %s: %w", txnID, err)
	}
	amt, err := domain.ParseCents(amtStr)
	if err != nil {
		return domain.TransferParty{}, err
	}
	p.Amount = amt
	p.OutCustName, p.InCustName = outName.String, inName.String
	return p, nil
}

// GetMerchant 查商户。
func (r *PaymentRepo) GetMerchant(ctx context.Context, merchantID string) (domain.Merchant, error) {
	var m domain.Merchant
	err := r.db.QueryRowContext(ctx, `SELECT merchant_id,merchant_name,mcc,region,status,create_biz_date
		FROM merchant WHERE merchant_id=$1`, merchantID).
		Scan(&m.MerchantID, &m.MerchantName, &m.MCC, &m.Region, &m.Status, &m.CreateBizDate)
	if err != nil {
		return domain.Merchant{}, fmt.Errorf("repo: 查商户 %s: %w", merchantID, err)
	}
	return m, nil
}

// scanTransfers 批量扫描转账行。
func scanTransfers(rows *sql.Rows) ([]domain.Transfer, error) {
	var out []domain.Transfer
	for rows.Next() {
		t, err := scanTransfer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// scanTransfer 单行扫描（scan 函数由 QueryRow 或 Rows 注入）。
func scanTransfer(scan func(dest ...any) error) (domain.Transfer, error) {
	var t domain.Transfer
	var amount, fee string
	var channel, counter, summary sql.NullString
	if err := scan(&t.TxnID, &t.BizDate, &t.OutAccount, &t.InAccount, &amount, &t.Ccy,
		&fee, &channel, &counter, &t.Status, &summary); err != nil {
		return domain.Transfer{}, err
	}
	amt, err := domain.ParseCents(amount)
	if err != nil {
		return domain.Transfer{}, err
	}
	f, err := domain.ParseCents(fee)
	if err != nil {
		return domain.Transfer{}, err
	}
	t.Amount, t.Fee = amt, f
	t.Channel, t.CounterBank, t.Summary = channel.String, counter.String, summary.String
	return t, nil
}
```

- [ ] **Step 4: 跑测试**

Run（需 postgres + `make seed` 含 setup_fdw）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./internal/payment/repo/...
```
Expected: PASS。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/internal/payment/repo/
git commit -m "feat(bank): payment repo with FDW cross-db join (transfer parties)"
```

---

## Task 11: payment service + api + cmd

**Files:**
- Create: `templates/bank/internal/payment/service/payment_service.go`
- Create: `templates/bank/internal/payment/api/handlers.go`
- Create: `templates/bank/internal/payment/api/router.go`
- Create: `templates/bank/internal/payment/api/handlers_test.go`
- Create: `templates/bank/cmd/payment/main.go`

**Interfaces:**
- Consumes: Task 10 的 `repo.PaymentRepo`
- Produces: `payment` 服务进程（:8082）

- [ ] **Step 1: 写 handler 单测（先失败）**

`templates/bank/internal/payment/api/handlers_test.go`:
```go
package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/payment/domain"
	"bank/internal/payment/service"
)

type fakePayRepo struct {
	transfer *domain.Transfer
	merchant *domain.Merchant
	parties  *domain.TransferParty
}

func (f fakePayRepo) ListTransfers(context.Context, string, string, string, int, int) ([]domain.Transfer, error) {
	return nil, nil
}
func (f fakePayRepo) GetTransfer(context.Context, string) (domain.Transfer, error) {
	if f.transfer != nil {
		return *f.transfer, nil
	}
	return domain.Transfer{}, sql.ErrNoRows
}
func (f fakePayRepo) GetTransferParties(context.Context, string) (domain.TransferParty, error) {
	if f.parties != nil {
		return *f.parties, nil
	}
	return domain.TransferParty{}, sql.ErrNoRows
}
func (f fakePayRepo) GetMerchant(context.Context, string) (domain.Merchant, error) {
	if f.merchant != nil {
		return *f.merchant, nil
	}
	return domain.Merchant{}, sql.ErrNoRows
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + path)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetTransfer_OK(t *testing.T) {
	h := &Handlers{Svc: service.NewPaymentService(fakePayRepo{transfer: &domain.Transfer{
		TxnID: "PT1", OutAccount: "D1", InAccount: "D2", Amount: domain.NewMoneyFromCents(100000),
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/payments/transfers/PT1")
	if code != 200 || !strings.Contains(body, `"amount":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetTransferParties(t *testing.T) {
	h := &Handlers{Svc: service.NewPaymentService(fakePayRepo{parties: &domain.TransferParty{
		TxnID: "PT1", OutAccount: "D1", OutCustName: "张伟", InAccount: "D2", InCustName: "李芳",
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/payments/transfers/PT1/parties")
	if code != 200 || !strings.Contains(body, `"out_cust_name":"张伟"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/payment/api/...`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现 service**

`templates/bank/internal/payment/service/payment_service.go`:
```go
package service

import (
	"context"

	"bank/internal/payment/domain"
)

type PaymentStore interface {
	ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error)
	GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error)
	GetTransferParties(ctx context.Context, txnID string) (domain.TransferParty, error)
	GetMerchant(ctx context.Context, merchantID string) (domain.Merchant, error)
}

type PaymentService struct{ store PaymentStore }

func NewPaymentService(store PaymentStore) *PaymentService { return &PaymentService{store: store} }

func (s *PaymentService) ListTransfers(ctx context.Context, accountNo, from, to string, limit, offset int) ([]domain.Transfer, error) {
	return s.store.ListTransfers(ctx, accountNo, from, to, limit, offset)
}
func (s *PaymentService) GetTransfer(ctx context.Context, txnID string) (domain.Transfer, error) {
	return s.store.GetTransfer(ctx, txnID)
}
func (s *PaymentService) GetParties(ctx context.Context, txnID string) (domain.TransferParty, error) {
	return s.store.GetTransferParties(ctx, txnID)
}
func (s *PaymentService) GetMerchant(ctx context.Context, id string) (domain.Merchant, error) {
	return s.store.GetMerchant(ctx, id)
}
```

- [ ] **Step 4: 实现 api**

`templates/bank/internal/payment/api/handlers.go`:
```go
// Package api 是 payment 服务的传输层：http handlers + chi router。
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/payment/service"

	"github.com/go-chi/chi/v5"
)

type Handlers struct{ Svc *service.PaymentService }

func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handlers) ListTransfers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	ts, err := h.Svc.ListTransfers(r.Context(), q.Get("account_no"), q.Get("from"), q.Get("to"), limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]transferResp, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTransferResp(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": out})
}

func (h *Handlers) GetTransfer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "txn_id")
	t, err := h.Svc.GetTransfer(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("转账不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, toTransferResp(t))
}

func (h *Handlers) GetTransferParties(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "txn_id")
	p, err := h.Svc.GetParties(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("转账不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, partiesResp{
		TxnID: p.TxnID, Amount: p.Amount.String(), Ccy: p.Ccy, BizDate: p.BizDate,
		OutAccount: p.OutAccount, OutCustName: p.OutCustName,
		InAccount: p.InAccount, InCustName: p.InCustName,
	})
}

func (h *Handlers) GetMerchant(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "merchant_id")
	m, err := h.Svc.GetMerchant(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("商户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, merchantResp{
		MerchantID: m.MerchantID, MerchantName: m.MerchantName, MCC: m.MCC,
		Region: m.Region, Status: m.Status, CreateBizDate: m.CreateBizDate,
	})
}

type transferResp struct {
	TxnID       string `json:"txn_id"`
	BizDate     string `json:"biz_date"`
	OutAccount  string `json:"out_account"`
	InAccount   string `json:"in_account"`
	Amount      string `json:"amount"`
	Ccy         string `json:"ccy"`
	Fee         string `json:"fee"`
	Channel     string `json:"channel,omitempty"`
	Summary     string `json:"summary,omitempty"`
}

type partiesResp struct {
	TxnID       string `json:"txn_id"`
	Amount      string `json:"amount"`
	Ccy         string `json:"ccy"`
	BizDate     string `json:"biz_date"`
	OutAccount  string `json:"out_account"`
	OutCustName string `json:"out_cust_name"`
	InAccount   string `json:"in_account"`
	InCustName  string `json:"in_cust_name"`
}

type merchantResp struct {
	MerchantID    string `json:"merchant_id"`
	MerchantName  string `json:"merchant_name"`
	MCC           string `json:"mcc"`
	Region        string `json:"region"`
	Status        string `json:"status"`
	CreateBizDate string `json:"create_biz_date"`
}

func toTransferResp(t interface{ getFields() }) transferResp { return transferResp{} } // 占位删除见下
```
> 上面 `toTransferResp` 占位会编译错。**用这版替换**（接收 `domain.Transfer`，需 import domain）:

`handlers.go` 顶部 import 加 `"bank/internal/payment/domain"`，`toTransferResp` 改为:
```go
func toTransferResp(t domain.Transfer) transferResp {
	return transferResp{
		TxnID: t.TxnID, BizDate: t.BizDate, OutAccount: t.OutAccount, InAccount: t.InAccount,
		Amount: t.Amount.String(), Ccy: t.Ccy, Fee: t.Fee.String(),
		Channel: t.Channel, Summary: t.Summary,
	}
}
```

`templates/bank/internal/payment/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/payments/transfers", h.ListTransfers)
		r.Get("/payments/transfers/{txn_id}", h.GetTransfer)
		r.Get("/payments/transfers/{txn_id}/parties", h.GetTransferParties)
		r.Get("/merchants/{merchant_id}", h.GetMerchant)
	})
	return r
}
```

`templates/bank/cmd/payment/main.go`（仿 customer/main.go，库名 pay_db、:8082）:
```go
// Package main 是 payment 只读 API 服务入口。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"bank/internal/payment/api"
	"bank/internal/payment/repo"
	"bank/internal/payment/service"
	"bank/internal/platform/pg"
)

func main() {
	dbName := getenv("DB_NAME", "pay_db")
	db, err := pg.Open(dbName)
	if err != nil {
		log.Fatalf("打开 %s 失败: %v", dbName, err)
	}
	defer db.Close()
	if err := waitForDB(db, 5, time.Second); err != nil {
		log.Fatalf("连 %s 失败: %v（请先 make up 再 make seed）", dbName, err)
	}
	handlers := &api.Handlers{Svc: service.NewPaymentService(repo.NewPaymentRepo(db))}
	port := getenv("API_PORT", "8082")
	srv := &http.Server{Addr: ":" + port, Handler: api.NewRouter(handlers)}
	go func() {
		log.Printf("payment 监听 :%s (db=%s)", port, dbName)
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
```

- [ ] **Step 5: 跑测试 + build**

Run:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/payment/...
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./cmd/payment
```
Expected: 测试 PASS；build 成功。

- [ ] **Step 6: Commit**
```bash
git add templates/bank/internal/payment/service/ templates/bank/internal/payment/api/ templates/bank/cmd/payment/
git commit -m "feat(bank): payment service + api + cmd (read-only, :8082)"
```

---

## Task 12: seed 完整编排（接入 customer/payment/fdw）

**Files:**
- Modify: `templates/bank/cmd/seed/main.go`

**Interfaces:**
- Consumes: Task 5/9 的生成器、Task 3 的 `fdw.SetupFDW`、core 的 `GenAccountRows`/`demand`
- Produces: seed 端到端建 3 库表 → core → customer → payment → fdw

- [ ] **Step 1: 用集成测试覆盖编排**（扩展 Task 2 的 `seed_test.go`，断言 seed 后三库有数据）

在 `templates/bank/cmd/seed/seed_test.go` 追加:
```go
func TestSeedRun_PopulatesAllDBs(t *testing.T) {
	ctx := context.Background()
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres，跳过: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪: %v", err)
	}
	// 直接调 main 的编排函数（需把编排逻辑抽成 run()，见 Step 3）
	if err := runSeed(ctx, fixtures.DefaultConfig(fixtures.ScaleDev), true); err != nil {
		t.Fatalf("runSeed 失败: %v", err)
	}
	for _, c := range []struct{ db, table string }{
		{"cust_db", "cust_info"}, {"cust_db", "cust_account_rel"},
		{"pay_db", "merchant"}, {"pay_db", "transfer_txn"}, {"pay_db", "consumption_txn"},
	} {
		db, err := pg.Open(c.db)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+c.table).Scan(&n)
		db.Close()
		if err != nil {
			t.Fatalf("查 %s.%s 失败（fdw/表未建？）: %v", c.db, c.table, err)
		}
		if n == 0 {
			t.Errorf("%s.%s 灌数据为空", c.db, c.table)
		}
	}
	// fdw 联邦表可查
	cust, _ := pg.Open("cust_db")
	defer cust.Close()
	if _, err := cust.ExecContext(ctx, "SELECT account_no FROM ext_core_db_demand_account LIMIT 1"); err != nil {
		t.Errorf("fdw 外部表不可查: %v", err)
	}
}
```
（需 import `"bank/internal/fixtures"`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./cmd/seed/...`
Expected: FAIL（`runSeed` undefined；占位段未真正生成 customer/payment）。

- [ ] **Step 3: 把 main 的编排抽成 runSeed 并接入 customer/payment/fdw**

改 `templates/bank/cmd/seed/main.go`：把 `main()` 的逻辑（建库→建表→core→…）抽成 `runSeed(ctx, cfg, reset) error`，`main()` 只解析 flag 调它；并把 4/5/6 占位替换为真实生成 + fdw。完整 `main.go`:

```go
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
	log.Println("[seed] 完成 ✅（3 库 + core + customer + payment + FDW）")
}

func runSeed(ctx context.Context, cfg fixtures.Config, reset bool) error {
	names := make([]string, len(allDBs))
	for i, d := range allDBs {
		names[i] = d.name
	}
	log.Println("[seed] 1/6 建 3 库")
	if err := ensureDBs(ctx, reset, names); err != nil {
		return fmt.Errorf("建库: %w（请先 make up）", err)
	}
	log.Println("[seed] 2/6 建 3 库表")
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

	log.Println("[seed] 3/6 core")
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
	if err := domains.WriteBalances(ctx, coreDB, domains.GenBalanceRows(cfg, demandNos)); err != nil {
		return err
	}
	if err := domains.WriteTxns(ctx, coreDB, domains.GenTxnRows(cfg, demandNos)); err != nil {
		return err
	}

	log.Println("[seed] 4/6 customer")
	// cust_id/account_no 编号规则与 core 一致 → 确定性关联
	nCustomers := cfg.TargetCounts().DemandAccounts / 2
	if nCustomers < 1 {
		nCustomers = 1
	}
	customers := domains.GenCustomers(cfg, nCustomers)
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

	log.Println("[seed] 5/6 payment")
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

	log.Println("[seed] 6/6 setup_fdw")
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
```

- [ ] **Step 4: 跑测试 + 端到端 seed**

Run:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags=integration ./cmd/seed/...
```
手动端到端（在 `templates/bank/`，需 postgres 起）:
```
make seed
```
Expected: 集成测试 PASS（三库有数据 + fdw 外部表可查）；`make seed` 日志打印 6 步 + 完成。

- [ ] **Step 5: Commit**
```bash
git add templates/bank/cmd/seed/main.go templates/bank/cmd/seed/seed_test.go
git commit -m "feat(bank): seed orchestration — core + customer + payment + fdw"
```

---

## Task 13: template.yaml + docker-compose + Makefile（多服务）

**Files:**
- Modify: `templates/bank/template.yaml`
- Modify: `templates/bank/docker-compose.yaml`
- Modify: `templates/bank/Makefile`
- Modify: `templates/bank/Dockerfile`（若需支持多 cmd build）

**Interfaces:**
- Produces: `docker compose up` 起 postgres + 3 服务；`template.yaml` 声明 3 库 3 服务

- [ ] **Step 1: 更新 template.yaml**

`templates/bank/template.yaml`:
```yaml
name: bank
description: 简化版银行核心系统（core/customer/payment 服务，Spec B-1）
version: 0.2.0
databases:
  - name: core_db
    migrate: db/migrations/core_db.sql
  - name: cust_db
    migrate: db/migrations/cust_db.sql
  - name: pay_db
    migrate: db/migrations/pay_db.sql
services:
  - name: core-banking
    port: 8080
    db: core_db
  - name: customer
    port: 8081
    db: cust_db
  - name: payment
    port: 8082
    db: pay_db
seed:
  entrypoint: go run ./cmd/seed
  scales: [dev, full]
```

- [ ] **Step 2: 更新 docker-compose.yaml**

`templates/bank/docker-compose.yaml`:
```yaml
services:
  postgres:
    image: postgres:16
    container_name: bank-postgres
    environment:
      POSTGRES_USER: bank
      POSTGRES_PASSWORD: bank
      POSTGRES_DB: postgres
    ports:
      - "5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U bank"]
      interval: 5s
      timeout: 3s
      retries: 10
  core-banking:
    build: .
    container_name: bank-core-banking
    restart: unless-stopped
    environment: &svcenv
      DB_HOST: postgres
      DB_PORT: "5432"
      DB_USER: bank
      DB_PASSWORD: bank
      DB_NAME: core_db
      API_PORT: "8080"
    ports: ["8080:8080"]
    depends_on:
      postgres: {condition: service_healthy}
  customer:
    build: .
    container_name: bank-customer
    restart: unless-stopped
    environment:
      <<: *svcenv
      DB_NAME: cust_db
      API_PORT: "8081"
    ports: ["8081:8081"]
    depends_on:
      postgres: {condition: service_healthy}
  payment:
    build: .
    container_name: bank-payment
    restart: unless-stopped
    environment:
      <<: *svcenv
      DB_NAME: pay_db
      API_PORT: "8082"
    ports: ["8082:8082"]
    depends_on:
      postgres: {condition: service_healthy}

volumes:
  pgdata:
```

- [ ] **Step 3: 更新 Dockerfile 支持指定 cmd**

检查 `templates/bank/Dockerfile`：Spec A 应是 `go build -o app ./cmd/core-banking`。改为参数化，默认 core-banking，使 customer/payment 容器各跑各的 cmd。新 Dockerfile:
```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG CMD=core-banking
RUN CGO_ENABLED=0 go build -o /app ./cmd/${CMD}

FROM alpine:latest
COPY --from=build /app /app
ENTRYPOINT ["/app"]
```
docker-compose 每个 service 加 `build: { context: ., args: {CMD: customer} }`（替换 Step 2 的 `build: .`）。更新 customer/payment 两个 service:
```yaml
  customer:
    build:
      context: .
      args:
        CMD: customer
    ...
  payment:
    build:
      context: .
      args:
        CMD: payment
    ...
```
（core-banking 保持 `build: .` 默认 CMD=core-banking，或也显式写 args。）

- [ ] **Step 4: 更新 Makefile（up 含两阶段：postgres→seed→服务）**

`templates/bank/Makefile`:
```makefile
.PHONY: up down seed test integration-test

up:
	docker compose up -d --build postgres
	$(MAKE) seed
	docker compose up -d --build core-banking customer payment

down:
	docker compose down

seed:
	go run ./cmd/seed --scale=$${SCALE:-dev} --reset

test:
	go test ./...

integration-test:
	go test -tags=integration ./...
```

- [ ] **Step 5: 本地验证 build + compose config**

Run（`templates/bank/`）:
```
CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./...
docker compose config >/dev/null
```
Expected: build 全绿；compose config 无语法错。（不在此 step 起容器，留给 Task 14 e2e。）

- [ ] **Step 6: Commit**
```bash
git add templates/bank/template.yaml templates/bank/docker-compose.yaml templates/bank/Dockerfile templates/bank/Makefile
git commit -m "feat(bank): multi-service compose + template.yaml + Dockerfile CMD arg"
```

---

## Task 14: e2e 冒烟 + 文档 + go generate 重新打包 + jiade 验收

**Files:**
- Modify: `templates/bank/README.md`、`templates/bank/ARCHITECTURE.md`
- Modify: jiade 仓 Makefile / CI（若 e2e 目标存在，扩展 curl）
- Regenerate: `internal/template/templates.tar`（jiade 仓）

**Interfaces:**
- 验收 spec §12 全部 7 条

- [ ] **Step 1: 更新 ARCHITECTURE.md**（加 customer/payment 服务节 + FDW 联邦说明）

在 `templates/bank/ARCHITECTURE.md` 追加「服务拓扑」节：3 进程（core-banking/customer/payment）+ 3 库（core_db/cust_db/pay_db）+ FDW 外部表映射图 + 2 个联邦端点说明。文字描述即可，无需代码。

- [ ] **Step 2: 更新 README.md**（curl 示例）

在 `templates/bank/README.md` 的用法节，把 Spec A 的单服务 curl 扩展为三服务 + 2 联邦端点示例:
```
curl localhost:8080/healthz                       # core-banking
curl localhost:8081/healthz                       # customer
curl localhost:8082/healthz                       # payment
curl localhost:8081/api/v1/customers/C0000001/accounts        # 跨库 FDW JOIN
curl localhost:8082/api/v1/payments/transfers/PT000000000001/parties  # 跨库 FDW JOIN
```

- [ ] **Step 3: 重新打包 templates.tar**（jiade 仓根）

Run（jiade 仓根 `/Users/yuhaochen/Documents/codebase/projanvil/Jiade`）:
```
go generate ./internal/template
```
Expected: `internal/template/templates.tar` 更新（含新文件）。`git status` 应显示 `internal/template/templates.tar` 修改。

- [ ] **Step 4: e2e 冒烟**（jiade 仓：init→up→seed→curl 三服务 + 2 联邦端点）

Run（jiade 仓根，需 docker）。若 jiade 仓 Makefile 有 `e2e` 目标，先扩展它；否则手动:
```
go build -o /tmp/jiade ./cmd/jiade
/tmp/jiade init --template bank --dir /tmp/e2e-bank --force
cd /tmp/e2e-bank
docker compose up -d --build postgres
go run ./cmd/seed --scale=dev --reset
docker compose up -d --build core-banking customer payment
sleep 3
curl -sf localhost:8080/healthz && curl -sf localhost:8081/healthz && curl -sf localhost:8082/healthz
curl -sf localhost:8081/api/v1/customers/C0000001/accounts
curl -sf localhost:8082/api/v1/payments/transfers/PT000000000001/parties
```
Expected: 三 healthz 200；联邦端点 200 且返回跨库数据（accounts 有账户行、parties 有客户姓名）。

若本地 5432 被占，临时 `docker stop bossy-postgres` 或用 `DB_PORT=5433` + 临时容器（CI 无此问题）。

- [ ] **Step 5: jiade 仓自验证（验收 #1）**

Run（jiade 仓根）:
```
go build ./...
go test ./...
```
Expected: 全绿（templates/ 不参与 jiade build，见 Task 14 已重新打包）。

- [ ] **Step 6: 验收对照**

对照 spec §12 七条，逐条确认：
1. jiade build/test 绿 ✓（Step 5）
2. templates/bank 独立 build/test 绿 ✓（各 Task 的 `go test ./...`）
3. init 产出含 customer/payment ✓（Step 4 init）
4. up→seed 后三 healthz 200 ✓（Step 4）
5. 2 联邦端点返回跨库数据 ✓（Step 4）
6. 确定性单测通过 ✓（Task 5/9）
7. 自包含 ✓（Step 4 用 `/tmp/e2e-bank` 脱离 jiade 源跑通）

- [ ] **Step 7: Commit**
```bash
git add templates/bank/README.md templates/bank/ARCHITECTURE.md internal/template/templates.tar
# 若改了 jiade Makefile/CI：
git add Makefile .github/workflows/
git commit -m "feat(bank): e2e smoke (3 services + FDW joins) + docs + repackage templates.tar"
```

- [ ] **Step 8: 追加 progress.md ledger**

在 `.superpowers/sdd/progress.md` 追加 Spec B-1 段（新建二级标题 `## Spec B-1`），按 Spec A 格式逐任务记录 `Task N: complete (commits <base7>..<head7>, review clean)`，并记录本计划的 Minor findings 供最终 whole-branch review。

---

## Self-Review Notes（写计划后自检，已修正）

- **spec 覆盖**：spec §2.1 六目标 → T1(schema) T2(多库) T3(fdw) T4-7(customer 四层) T8-11(payment 四层) T12(编排) T13(compose/template) T14(e2e/验收) 全覆盖；spec §2.2 非目标（写接口/切日/剩余服务）明确不在本计划。
- **占位**：无 TBD；Task 3 的 fdw.go 初稿用了冗余 `pgConn` interface（`*pgConn` 指向 interface 会编译错）且漏 `database/sql` import，已修正为 `setupOne(ctx, db *sql.DB, m)` + import `database/sql` + 删除 `pgConn`/`pgEnv`；Task 7/11 的 handlers.go 占位 `customerReader`/`toTransferResp` 已在该 Task 内给修正版（用 `domain.Transfer` + import domain）。
- **类型一致**：`CustomerRepo.GetCustAccounts` 返回 `[]domain.CustAccount`（Task 4 定义）↔ service `Accounts` ↔ handler `GetCustAccounts` 一致；`PaymentRepo.GetTransferParties` 返回 `domain.TransferParty`（Task 8 定义）↔ service `GetParties` ↔ handler 一致；`TransferParty.OutCustName` JSON tag `out_cust_name` 与测试断言一致。
- **fdw 顺序**：Task 3（fdw）排在 Task 6/10（repo FDW JOIN）之前，使 repo 集成测试可在 setup_fdw 后跑（依赖外部表已建）。
- **Money 禁 float**：Task 8 的 `TestSourceHasNoFloat` 守卫读 `money.go`，若读不到会 `t.Fatal`（修正了 Spec A Minor finding 里 `t.Skip` 静默失效的问题）。
