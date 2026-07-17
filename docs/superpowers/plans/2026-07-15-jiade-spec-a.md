# Jiade Spec A 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建一个 Go/Cobra CLI（`jiade`）与一份地道的银行纵切模板（`bank`），使 `jiade init --template bank` 能拷贝出一个自包含、可 `docker compose up` + `go run ./cmd/seed` 运行的真实分层 Go 工程。

**Architecture:** 双层布局。第 1 层是 `jiade` 工具仓（module `github.com/projanvil/jiade`）：Cobra CLI（list/init/up/down/seed）+ docker 探测 + 模板 registry + 逐字拷贝渲染。第 2 层是 `bank` 模板（独立 module `bank`）：core-banking 单服务按 `api → service → repo → domain` 分层 + 复式记账不变量 + Go fixture 生成器 + docker-compose（postgres + core-banking）。模板通过 `go:embed` 作为静态资源嵌入 jiade 二进制，**不**参与 jiade 自身编译。

**Tech Stack:** Go 1.22、Cobra、chi v5、database/sql + pgx/v5、postgres:16、go:embed、promptui。

## Global Constraints

以下约束逐字来自设计文档 `docs/superpowers/specs/2026-07-15-jiade-spec-a-design.md`，每个任务的隐含需求都包含本节：

- **金额用 int64 分**，禁止 float（设计文档 §7.2）。DB 层保留 `NUMERIC(18,2)`；repo 层负责 `分 ↔ NUMERIC 字符串` 的纯整数转换。
- **init 纯 copy-paste**：逐字拷贝，**v1 零字符串替换**（§5.1）。`--dir` 指工程根目录本身（文件直接落在 `<dir>/` 下）；不存在则创建；已存在且非空 → 拒绝，`--force` 才覆盖。
- **生成物自包含**：拷出的工程离开 `jiade` 也能 `docker compose up` 和 `go run`；`jiade` 是薄编排器，非运行时依赖（§3）。
- **Jiade 自闭**：不探测/调用/引导/包装 SCV/Porto，无 `doctor` 命令（§1.2、§2.2）。
- **模块边界**：`templates/bank/` 是独立 Go module（`go.mod: module bank`），被 `go:embed` 嵌入为静态资源，**不**参与 jiade 的 `go build ./...`（§5.2）。其正确性靠"拷出来再 build/test"保证。
- **金额之外的 decimal**（如定期 `rate NUMERIC(10,6)`）：domain 用 `string` 透传，不做 float 运算。
- **破坏性操作显式确认**：`--reset`（seed 重建库）与 `--force`（init 覆盖）是仅有的删除机制；无此二者，jiade 不删任何东西（§10）。
- **子进程 stderr 透传**：`up`/`seed` 不吞子进程 stderr，退出码透传（§10）。
- **module path**：jiade module = `github.com/projanvil/jiade`；bank 模板 module = `bank`。
- **Go 版本**：`go 1.22`（`math/rand/v2` 在 1.22 稳定，用于确定性 fixture）。
- **embed 机制（执行中修正）**：原计划 `//go:embed all:templates` **无法**嵌入嵌套 module（`templates/bank/go.mod`——实证报 `contains no embeddable files`）。改用 `//go:embed templates.tar`（单文件，`go:generate` 从 `templates/bank` 打包）+ `init` 用 `archive/tar` 解压（逐字、零替换，符合 §5.1 纯 copy）。`templates.tar` 不入 git（`.gitignore`）；任何 jiade 的 `go build`/`go test` 前**必须** `go generate ./internal/template`（或 `tar -C templates -cf internal/template/templates.tar bank`）重新打包。bank 模板原地真实 `go.mod` 保留不变。

## Scope Check（关于单一计划）

Spec A 含两个子系统：jiade CLI 与 bank 模板。它们能各自产出可工作软件（bank 模板可独立 `docker compose up`+`go run`；CLI 的 registry/list 可独立测），但 **bank 是 jiade `init` 的嵌入依赖**，且共享验收标准 #5（`init → up → seed → curl`）。拆成两个计划会产生交接复杂度，故合并为单计划，分 Phase：先 bank 模板（Phase 1），再 CLI（Phase 2），最后端到端（Phase 3）。

## 文件结构

### 第 1 层 — jiade 工具仓（本计划 Phase 0 + 2 + 3 产出）

| 文件 | 职责 |
|------|------|
| `go.mod` | module `github.com/projanvil/jiade`，go 1.22 |
| `cmd/jiade/main.go` | CLI 入口，装配 root + 子命令 |
| `internal/cli/root.go` | Cobra root 命令 + 全局 flag 解析（`--dir`） |
| `internal/cli/list.go` | `jiade list` |
| `internal/cli/init.go` | `jiade init`（交互 + copy） |
| `internal/cli/compose.go` | `jiade up` / `jiade down`（docker 探测 + compose） |
| `internal/cli/seed.go` | `jiade seed`（跑目标 fixture） |
| `internal/template/manifest.go` | `template.yaml` 解析为 `Manifest` 结构 |
| `internal/template/registry.go` | 发现 `templates/` 下的模板，go:embed |
| `internal/template/render.go` | 逐字 copy（非空拒绝 / `--force` 覆盖） |
| `internal/template/manifest_test.go` / `render_test.go` | 单测 |
| `internal/docker/probe.go` | 探测 docker / docker compose / daemon（接口抽象） |
| `internal/docker/probe_test.go` | 单测（假 executor，不依赖真 docker） |
| `internal/ui/ui.go` | 输出着色（成功/警告/错误/步骤） |
| `templates/bank/...` | 第 2 层源（Phase 1 产出，被 embed） |
| `README.md` | jiade 仓说明 |

### 第 2 层 — bank 模板（Phase 1 产出，独立 module `bank`）

| 文件 | 职责 |
|------|------|
| `templates/bank/go.mod` | module `bank`，go 1.22 |
| `templates/bank/go.sum` | 依赖校验 |
| `templates/bank/template.yaml` | 模板清单（被 list/up/seed 读） |
| `templates/bank/docker-compose.yaml` | postgres:16 + core-banking |
| `templates/bank/.env.example` | DB/端口配置 |
| `templates/bank/Makefile` | up/down/seed/test（不装 jiade 也能跑） |
| `templates/bank/README.md` / `ARCHITECTURE.md` | 工程说明 |
| `templates/bank/db/migrations/core_db.sql` | 核心账务库 DDL（10 表） |
| `templates/bank/cmd/core-banking/main.go` | API 服务入口 |
| `templates/bank/cmd/seed/main.go` | fixture 生成器入口 |
| `templates/bank/internal/platform/pg/pg.go` | 连接池 + DSN |
| `templates/bank/internal/platform/migrate/migrate.go` | 跑 core_db.sql（按分号切分） |
| `templates/bank/internal/corebanking/domain/money.go` | `Money`（int64 分）+ 解析/格式化 |
| `templates/bank/internal/corebanking/domain/account.go` | 活期/定存账户 + 状态机 |
| `templates/bank/internal/corebanking/domain/ledger.go` | 科目 / 借贷标志 / Entry / 总账 |
| `templates/bank/internal/corebanking/domain/txn.go` | 流水 / 余额 |
| `templates/bank/internal/corebanking/service/ledger_service.go` | 复式记账 `Post`（借必等于贷） |
| `templates/bank/internal/corebanking/service/account_service.go` | 开户/销户/状态流转 |
| `templates/bank/internal/corebanking/service/txn_service.go` | 记账/冲正 |
| `templates/bank/internal/corebanking/repo/account_repo.go` | 账户/余额落库（pgx raw SQL） |
| `templates/bank/internal/corebanking/repo/txn_repo.go` | 流水落库 |
| `templates/bank/internal/corebanking/repo/ledger_repo.go` | 总账落库 + 复式记账持久化 |
| `templates/bank/internal/corebanking/api/handlers.go` | health/account/balance/txn/ledger handlers |
| `templates/bank/internal/corebanking/api/router.go` | chi 路由 |
| `templates/bank/internal/fixtures/config.go` | Scale / TargetCounts / Config |
| `templates/bank/internal/fixtures/rng.go` | 确定性 RNG + 手写词库 |
| `templates/bank/internal/fixtures/domains/core.go` | GenStatic/GenAccounts/基线balance/少量txn |
| `templates/bank/internal/fixtures/domains/core_test.go` | 确定性单测 |

---

## Phase 0 — jiade 仓骨架

### Task 1: jiade Go module + Cobra 骨架

**Files:**
- Create: `go.mod`
- Create: `cmd/jiade/main.go`
- Create: `internal/cli/root.go`
- Create: `internal/cli/list.go`
- Create: `internal/cli/init.go`
- Create: `internal/cli/compose.go`
- Create: `internal/cli/seed.go`
- Create: `README.md`

**Interfaces:**
- Produces: `cli.New()` 返回装配好子命令的 `*cobra.Command`；全局 `--dir` flag。后续任务向 `root.go` 注入真实命令体（list/init/up/down/seed 当前为桩，Task 16/17 替换实现）。

- [ ] **Step 1: 初始化 module 与依赖**

Run:
```bash
cd .
go mod init github.com/projanvil/jiade
go get github.com/spf13/cobra@v1.8.1
go get github.com/manifoldco/promptui@v0.9.0
go mod tidy
```
Expected: `go.mod` 生成，`require` 含 cobra 与 promptui；`go.sum` 出现。

- [ ] **Step 2: 写 root 命令**

Create `internal/cli/root.go`:
```go
package cli

import (
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Options 持有跨子命令共享的全局状态。
type Options struct {
	Dir   string // 目标工程根目录（--dir）
	Stdout io.Writer
	Stderr io.Writer
}

// New 返回装配好子命令的 jiade root 命令。
func New() *cobra.Command {
	opts := &Options{Stdout: os.Stdout, Stderr: os.Stderr}
	root := &cobra.Command{
		Use:   "jiade",
		Short: "生成「现实世界大工程的缩影」——可运行的行业 Go 工程",
	}
	root.PersistentFlags().StringVar(&opts.Dir, "dir", "", "目标工程根目录")
	root.AddCommand(newListCmd(opts))
	root.AddCommand(newInitCmd(opts))
	root.AddCommand(newUpCmd(opts))
	root.AddCommand(newDownCmd(opts))
	root.AddCommand(newSeedCmd(opts))
	return root
}
```

- [ ] **Step 3: 写五个子命令桩（Task 16/17 替换为真实实现）**

Create `internal/cli/list.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出可用模板",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("list: 尚未实现（见 Task 16）")
		},
	}
}
```

Create `internal/cli/init.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newInitCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "从模板拷贝出一个工程（逐字拷贝，零替换）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("init: 尚未实现（见 Task 17）")
		},
	}
	cmd.Flags().String("template", "", "模板名（如 bank）")
	cmd.Flags().Bool("force", false, "目标目录非空时强制覆盖")
	return cmd
}
```

Create `internal/cli/compose.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "在目标目录内 docker compose up -d（前置探测 docker）",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("up: 尚未实现（见 Task 18）")
		},
	}
	cmd.Flags().Bool("build", false, "compose up 时强制 --build")
	return cmd
}

func newDownCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "在目标目录内 docker compose down",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("down: 尚未实现（见 Task 18）")
		},
	}
}
```

Create `internal/cli/seed.go`:
```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newSeedCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "运行目标工程的 fixture 生成器",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("seed: 尚未实现（见 Task 18）")
		},
	}
	cmd.Flags().String("scale", "dev", "规模：dev|full")
	cmd.Flags().Bool("reset", false, "重建库与表（幂等）")
	return cmd
}
```

- [ ] **Step 4: 写 main 入口**

Create `cmd/jiade/main.go`:
```go
package main

import (
	"fmt"
	"os"

	"github.com/projanvil/jiade/internal/cli"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "jiade:", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 5: 验证编译与帮助输出**

Run:
```bash
go build ./...
go run ./cmd/jiade --help
```
Expected: `go build` 无错；`--help` 打印 `list/init/up/down/seed` 子命令。

- [ ] **Step 6: 写 README 骨架**

Create `README.md`:
```markdown
# jiade

生成「现实世界大工程的缩影」——可运行的行业 Go 工程。

## 安装

```bash
go install github.com/projanvil/jiade/cmd/jiade@latest
```

## 快速开始

```bash
jiade init --template bank --dir ./mybank
cd mybank
jiade up      # docker compose up -d
jiade seed    # go run ./cmd/seed --scale=dev --reset
```

生成物自包含：未安装 jiade 也可用 `make up && make seed`（见模板内 Makefile）。

Jiade 自闭：不与 SCV/Porto 联动，无 doctor 命令。
```

- [ ] **Step 7: 提交**

```bash
git add go.mod go.sum cmd internal README.md
git commit -m "feat: scaffold jiade CLI with cobra command tree

Co-Authored-By: Claude <noreply@anthropic.com>"
```

## Phase 1 — bank 模板纵切（独立 module `bank`）

> 所有 Phase 1 任务的工作目录为 `templates/bank/`（它是独立 Go module，命令需 `cd templates/bank` 或在其中执行）。

### Task 2: bank module 骨架 + docker-compose + 迁移 SQL

**Files:**
- Create: `templates/bank/go.mod`
- Create: `templates/bank/docker-compose.yaml`
- Create: `templates/bank/Dockerfile`
- Create: `templates/bank/.env.example`
- Create: `templates/bank/Makefile`
- Create: `templates/bank/template.yaml`
- Create: `templates/bank/db/migrations/core_db.sql`
- Create: `templates/bank/README.md`
- Create: `templates/bank/ARCHITECTURE.md`
- Create: `templates/bank/.gitignore`

**Interfaces:**
- Produces: 一个可 `go build ./...`（暂无 .go 源码则无包，但 go.mod 合法）的独立 module；`docker-compose.yaml` 定义 `postgres:16`（user/pass/db=`bank`）与 `core-banking`（`build: .`，注入 `DB_*` 环境变量，端口 8080）；`core_db.sql` 含 10 张表（后续 Task 3-13 的 domain/repo/service 全部对应这些表）。
- Produces（SQL 表契约，后续任务严格遵循列名）：

| 表 | PK | 关键列 |
|----|----|--------|
| `sys_param` | `param_key` | `param_value` |
| `ccy` | `ccy_code` | `ccy_name,decimal_digits,status` |
| `chart_of_acct` | `subject_code` | `subject_name,dc_attr,level,parent_subject,status` |
| `interest_rate` | `rate_id` | `acct_type,ccy,rate_value NUMERIC(10,6),effective_biz_date,status` |
| `branch` | `branch_code` | `branch_name,parent_branch,region,level,status` |
| `demand_account` | `account_no` | `cust_id,ccy,acct_status,open_biz_date,branch_code,product_code,subject_code` |
| `fixed_account` | `account_no` | `cust_id,ccy,principal NUMERIC(18,2),rate NUMERIC(10,6),term_months,start_biz_date,mature_date,acct_status,subject_code` |
| `account_balance` | `(account_no,biz_date)` | `balance,available_balance,frozen_amount NUMERIC(18,2),subject_code` |
| `acct_txn` | `txn_id` | `biz_date,txn_ts,account_no,dc_flag,amount NUMERIC(18,2),ccy,subject_code,opp_account,ref_txn_id,channel,summary` |
| `gl_balance` | `(subject_code,biz_date,ccy)` | `dc_balance,cc_balance NUMERIC(18,2)` |

- [ ] **Step 1: 初始化 bank module 并拉依赖**

Run:
```bash
cd ./templates/bank
go mod init bank
go get github.com/go-chi/chi/v5@v5.0.12
go get github.com/jackc/pgx/v5@v5.5.5
go mod tidy
```
Expected: `templates/bank/go.mod` 含 `module bank`、`go 1.22`；`go.sum` 生成。

- [ ] **Step 2: 写 core_db.sql（10 表）**

Create `templates/bank/db/migrations/core_db.sql`:
```sql
CREATE TABLE IF NOT EXISTS sys_param (
    param_key   TEXT PRIMARY KEY,
    param_value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ccy (
    ccy_code       TEXT PRIMARY KEY,
    ccy_name       TEXT NOT NULL,
    decimal_digits INTEGER DEFAULT 2,
    status         TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS chart_of_acct (
    subject_code   TEXT PRIMARY KEY,
    subject_name   TEXT NOT NULL,
    dc_attr        TEXT NOT NULL,            -- 借/贷
    level          INTEGER,
    parent_subject TEXT,
    status         TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS interest_rate (
    rate_id            TEXT PRIMARY KEY,
    acct_type          TEXT NOT NULL,
    ccy                TEXT NOT NULL,
    rate_value         NUMERIC(10,6) NOT NULL,
    effective_biz_date DATE NOT NULL,
    status             TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS branch (
    branch_code   TEXT PRIMARY KEY,
    branch_name   TEXT NOT NULL,
    parent_branch TEXT,
    region        TEXT,
    level         INTEGER,
    status        TEXT DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS demand_account (
    account_no     TEXT PRIMARY KEY,
    cust_id        TEXT NOT NULL,
    ccy            TEXT NOT NULL,
    acct_status    TEXT DEFAULT 'active',
    open_biz_date  DATE NOT NULL,
    branch_code    TEXT,
    product_code   TEXT,
    subject_code   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS fixed_account (
    account_no     TEXT PRIMARY KEY,
    cust_id        TEXT NOT NULL,
    ccy            TEXT NOT NULL,
    principal      NUMERIC(18,2) NOT NULL,
    rate           NUMERIC(10,6) NOT NULL,
    term_months    INTEGER NOT NULL,
    start_biz_date DATE NOT NULL,
    mature_date    DATE NOT NULL,
    acct_status    TEXT DEFAULT 'active',
    subject_code   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS account_balance (
    account_no        TEXT NOT NULL,
    biz_date          DATE NOT NULL,
    balance           NUMERIC(18,2) NOT NULL,
    available_balance NUMERIC(18,2) NOT NULL,
    frozen_amount     NUMERIC(18,2) DEFAULT 0,
    subject_code      TEXT,
    PRIMARY KEY (account_no, biz_date)
);

CREATE TABLE IF NOT EXISTS acct_txn (
    txn_id        TEXT PRIMARY KEY,
    biz_date      DATE NOT NULL,
    txn_ts        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    account_no    TEXT NOT NULL,
    dc_flag       TEXT NOT NULL,             -- 借/贷
    amount        NUMERIC(18,2) NOT NULL,
    ccy           TEXT NOT NULL,
    subject_code  TEXT NOT NULL,
    opp_account   TEXT,
    ref_txn_id    TEXT,
    channel       TEXT,
    summary       TEXT
);
CREATE INDEX IF NOT EXISTS idx_acct_txn_bizdate ON acct_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_acct_txn_acct ON acct_txn(account_no, biz_date);

CREATE TABLE IF NOT EXISTS gl_balance (
    subject_code TEXT NOT NULL,
    biz_date     DATE NOT NULL,
    dc_balance   NUMERIC(18,2) DEFAULT 0,
    cc_balance   NUMERIC(18,2) DEFAULT 0,
    ccy          TEXT NOT NULL,
    PRIMARY KEY (subject_code, biz_date, ccy)
);
```

- [ ] **Step 3: 写 docker-compose.yaml + Dockerfile**

Create `templates/bank/docker-compose.yaml`:
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
    environment:
      DB_HOST: postgres
      DB_PORT: "5432"
      DB_USER: bank
      DB_PASSWORD: bank
      DB_NAME: core_db
      API_PORT: "8080"
    ports:
      - "8080:8080"
    depends_on:
      postgres:
        condition: service_healthy

volumes:
  pgdata:
```

Create `templates/bank/Dockerfile`:
```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /core-banking ./cmd/core-banking

FROM alpine:3.19
COPY --from=build /core-banking /core-banking
EXPOSE 8080
ENTRYPOINT ["/core-banking"]
```

- [ ] **Step 4: 写 .env.example / Makefile / template.yaml**

Create `templates/bank/.env.example`:
```env
# bank 工程运行配置（复制为 .env 后按需修改）
DB_HOST=localhost
DB_PORT=5432
DB_USER=bank
DB_PASSWORD=bank
DB_NAME=core_db
API_PORT=8080
```

Create `templates/bank/Makefile`:
```makefile
.PHONY: up down seed test integration-test

up:
	docker compose up -d --build

down:
	docker compose down

seed:
	go run ./cmd/seed --scale=$${SCALE:-dev} --reset

test:
	go test ./...

integration-test:
	go test -tags=integration ./...
```

Create `templates/bank/template.yaml`:
```yaml
name: bank
description: 简化版银行核心系统（core-banking 服务，Spec A 纵切）
version: 0.1.0
databases:
  - name: core_db
    migrate: db/migrations/core_db.sql
services:
  - name: core-banking
    port: 8080
    db: core_db
seed:
  entrypoint: go run ./cmd/seed
  scales: [dev, full]
```

- [ ] **Step 5: 写 README.md / ARCHITECTURE.md / .gitignore**

Create `templates/bank/README.md`:
```markdown
# bank（jiade 模板：core-banking 纵切）

简化版银行核心系统——「现实世界大工程的缩影」。本工程由 `jiade init --template bank` 生成，**自包含**：离开 jiade 也可独立运行。

## 快速开始

```bash
make up       # docker compose up -d（postgres + core-banking）
make seed     # 建库 → 建表 → 灌 fixture（幂等：--reset）
curl localhost:8080/healthz
curl localhost:8080/api/v1/accounts/D0000000001
```

## 架构

见 [ARCHITECTURE.md](ARCHITECTURE.md)。分层 `api → service → repo → domain`，domain 零外部依赖。

## 金融不变量

- 金额用 int64 分表示，禁 float。
- 复式记账：过账强制 sum(借)==sum(贷)，不平回滚。
```

Create `templates/bank/ARCHITECTURE.md`:
```markdown
# bank 架构

## 分层

```
internal/
├── platform/          基础设施（pg 连接 + migration runner）
├── corebanking/
│   ├── domain/        纯领域模型（零 DB/框架依赖，最内层）
│   ├── repo/          仓储层（pgx raw SQL 落库）
│   ├── service/       用例层（业务规则，纯逻辑可单测）
│   └── api/           传输层（http handlers + chi router）
└── fixtures/          Go fixture 生成器（确定性）
```

依赖方向向内：`api → service → repo → domain`，`domain` 不依赖任何人。

## 数据流

- `cmd/seed`：连 postgres 管理库 → 建 core_db → 跑 core_db.sql → 灌静态主数据 + 账户 + 基线余额 + 少量流水。
- `cmd/core-banking`：连 core_db（只读），暴露只读 HTTP API。

## Spec A 范围

core-banking 单服务（活期/定存 + 复式记账 + 总账）。其余 6 服务、多日切日引擎、写 HTTP 接口留 Spec B。
```

Create `templates/bank/.gitignore`:
```
.env
/core-banking
```

- [ ] **Step 6: 验证 module 合法**

Run:
```bash
cd ./templates/bank
go build ./... 2>&1 || true   # 暂无 .go 源码，预期 "no Go files" 或成功
go vet ./... 2>&1 || true
cat go.mod
```
Expected: `go build` 报 `no Go files in .../templates/bank`（合法，后续任务补源码）；`go.mod` 含 `module bank`。

- [ ] **Step 7: 提交**

```bash
cd .
git add templates/bank
git commit -m "feat(bank): scaffold bank module with compose, sql, docs

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: platform 层 — pg 连接 + migration runner

**Files:**
- Create: `templates/bank/internal/platform/pg/pg.go`
- Create: `templates/bank/internal/platform/migrate/migrate.go`
- Create: `templates/bank/internal/platform/migrate/migrate_test.go`

**Interfaces:**
- Produces: `pg.DSN(dbName string) string`、`pg.Open(dbName string) (*sql.DB, error)`；`migrate.Run(ctx, db, ddl) error`、`migrate.SplitStatements(sqlText) []string`。
- DB 连接从环境变量 `DB_HOST/DB_PORT/DB_USER/DB_PASSWORD` 读取，默认 `localhost:5432 bank/bank`（使生成物无 .env 也能跑）。`DB_NAME` 通过参数传入（seed 用 `postgres` 建库、`core_db` 用库）。

- [ ] **Step 1: 写失败的 migrate 单测**

Create `templates/bank/internal/platform/migrate/migrate_test.go`:
```go
package migrate

import (
	"strings"
	"testing"
)

func TestSplitStatements_DropsEmptyAndTrims(t *testing.T) {
	ddl := "  CREATE TABLE a(x int);\n\n;  CREATE TABLE b(y int);  "
	got := SplitStatements(ddl)
	want := 2
	if len(got) != want {
		t.Fatalf("want %d statements, got %d: %#v", want, len(got), got)
	}
	if strings.Contains(got[0], ";") {
		t.Errorf("statement should not contain trailing semicolon: %q", got[0])
	}
}

func TestSplitStatements_Empty(t *testing.T) {
	if got := SplitStatements("  ;  \n; "); len(got) != 0 {
		t.Errorf("want 0 statements, got %d", len(got))
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```bash
cd ./templates/bank
go test ./internal/platform/migrate/...
```
Expected: FAIL（`SplitStatements` 未定义）。

- [ ] **Step 3: 写 migrate 实现**

Create `templates/bank/internal/platform/migrate/migrate.go`:
```go
// Package migrate 把 DDL 文本应用到已存在的数据库。
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Run 执行 DDL 文本（按分号切分语句，逐条执行）。
func Run(ctx context.Context, db *sql.DB, ddl string) error {
	for _, stmt := range SplitStatements(ddl) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: 执行失败 %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// SplitStatements 按分号切分 SQL 语句（core_db.sql 无嵌套分号，安全）。
func SplitStatements(sqlText string) []string {
	var out []string
	for _, s := range strings.Split(sqlText, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/platform/migrate/...
```
Expected: PASS。

- [ ] **Step 5: 写 pg 连接包**

Create `templates/bank/internal/platform/pg/pg.go`:
```go
// Package pg 提供 PostgreSQL 连接构造。
package pg

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 pgx 到 database/sql
)

// DSN 从环境变量构造连接串。dbName 指定连哪个库（postgres / core_db）。
func DSN(dbName string) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		getenv("DB_USER", "bank"),
		getenv("DB_PASSWORD", "bank"),
		getenv("DB_HOST", "localhost"),
		getenv("DB_PORT", "5432"),
		dbName,
	)
}

// Open 打开一个到 dbName 的连接池。调用方负责 Close。
func Open(dbName string) (*sql.DB, error) {
	db, err := sql.Open("pgx", DSN(dbName))
	if err != nil {
		return nil, fmt.Errorf("pg: 打开 %s 失败: %w", dbName, err)
	}
	return db, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 6: 验证编译**

Run:
```bash
cd ./templates/bank
go build ./...
go test ./internal/platform/...
```
Expected: 编译通过；migrate 测试 PASS。

- [ ] **Step 7: 提交**

```bash
cd .
git add templates/bank/internal/platform templates/bank/go.mod templates/bank/go.sum
git commit -m "feat(bank): add pg connection + migration runner

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 4: domain 层 — Money（int64 分，禁 float）

**Files:**
- Create: `templates/bank/internal/corebanking/domain/money.go`
- Create: `templates/bank/internal/corebanking/domain/money_test.go`

**Interfaces:**
- Produces: `domain.Money`（`int64` 分）；`domain.NewMoneyFromCents(int64) Money`；`domain.ParseCents(string) (Money, error)`；`Money.Add/Sub/Cents/String`。**无任何 float 入口**（金融不变量）。repo 层用 `ParseCents` 读 NUMERIC、用 `String()` 写 NUMERIC。

- [ ] **Step 1: 写失败的 money 单测**

Create `templates/bank/internal/corebanking/domain/money_test.go`:
```go
package domain

import (
	"strings"
	"testing"
)

func TestParseCents(t *testing.T) {
	cases := []struct {
		in   string
		want Money
	}{
		{"1234.56", 123456},
		{"1234", 123400},
		{"1234.5", 123450},
		{"0.01", 1},
		{"0", 0},
		{"-1.50", -150},
	}
	for _, c := range cases {
		got, err := ParseCents(c.in)
		if err != nil {
			t.Fatalf("ParseCents(%q) 非预期错误: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseCents(%q)=%d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseCents_TooManyDecimals(t *testing.T) {
	if _, err := ParseCents("1234.567"); err == nil {
		t.Error("超过 2 位小数应报错")
	}
}

func TestMoneyString(t *testing.T) {
	cases := []struct {
		m    Money
		want string
	}{
		{123456, "1234.56"},
		{123400, "1234.00"},
		{1, "0.01"},
		{0, "0.00"},
		{-150, "-1.50"},
	}
	for _, c := range cases {
		if got := c.m.String(); got != c.want {
			t.Errorf("Money(%d).String()=%q, want %q", c.m, got, c.want)
		}
	}
}

func TestMoneyRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00", "99.99", "1234567.89", "-0.50"} {
		m, err := ParseCents(s)
		if err != nil {
			t.Fatalf("ParseCents(%q): %v", s, err)
		}
		if got := m.String(); got != s {
			t.Errorf("round-trip: ParseCents(%q).String()=%q", s, got)
		}
	}
}

func TestMoneyAddSub(t *testing.T) {
	if got := (Money(100)).Add(Money(50)); got != 150 {
		t.Errorf("Add=%d, want 150", got)
	}
	if got := (Money(100)).Sub(Money(30)); got != 70 {
		t.Errorf("Sub=%d, want 70", got)
	}
}

// 禁 float 守卫：源码不得出现 float32/float64 关键字（防回归）。
func TestSourceHasNoFloat(t *testing.T) {
	src, err := readFile("money.go")
	if err != nil {
		t.Skip("readFile 仅在测试目录可用，跳过守卫")
	}
	if strings.Contains(src, "float32") || strings.Contains(src, "float64") {
		t.Error("money.go 禁止使用 float")
	}
}

func readFile(name string) (string, error) {
	b, err := osReadFile(name)
	return string(b), err
}
```
> 注：最后这个 float 守卫测试用 `osReadFile` 别名以保持包内整洁；在 Step 3 的实现里会 `import "os"` 并定义 `var osReadFile = os.ReadFile`。

- [ ] **Step 2: 运行测试确认失败**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/domain/...
```
Expected: FAIL（`Money`、`ParseCents` 未定义）。

- [ ] **Step 3: 写 money 实现**

Create `templates/bank/internal/corebanking/domain/money.go`:
```go
// Package domain 是 core-banking 的纯领域模型，零 DB/框架依赖（最内层）。
package domain

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

var osReadFile = os.ReadFile

// Money 用 int64 分表示金额。金融禁 float。
// 构造仅经 NewMoneyFromCents 或 ParseCents——无 float 入口。
type Money int64

// NewMoneyFromCents 直接由分构造（推荐入口，无 float）。
func NewMoneyFromCents(cents int64) Money { return Money(cents) }

// ParseCents 把 NUMERIC(18,2) 字符串（如 "1234.56"）解析为分（123456）。
// 纯整数运算，杜绝 float 精度问题。
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

// Add 返回 m+o。
func (m Money) Add(o Money) Money { return m + o }

// Sub 返回 m-o。
func (m Money) Sub(o Money) Money { return m - o }

// Cents 返回分值。
func (m Money) Cents() int64 { return int64(m) }

// String 返回 NUMERIC(18,2) 风格字符串（写入 DB 用），如 "1234.56"。
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

- [ ] **Step 4: 运行测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/domain/...
```
Expected: PASS（含 float 守卫）。

- [ ] **Step 5: 提交**

```bash
cd .
git add templates/bank/internal/corebanking/domain
git commit -m "feat(bank): add Money domain type (int64 cents, float-free)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 5: domain 层 — 账户/科目/借贷/流水/余额

**Files:**
- Create: `templates/bank/internal/corebanking/domain/account.go`
- Create: `templates/bank/internal/corebanking/domain/account_test.go`
- Create: `templates/bank/internal/corebanking/domain/ledger.go`
- Create: `templates/bank/internal/corebanking/domain/txn.go`

**Interfaces:**
- Produces: `AccountStatus`（active/closed/frozen）+ 状态机函数 `Close/Freeze/Unfreeze`；`DemandAccount`/`FixedAccount` 结构；`DCFlag`（"借"/"贷"）；`Subject`；`Entry{AccountNo,DCFlag,Amount,SubjectCode}`；`GLBalance`；`Txn`；`Balance`。这些类型被 service（Task 6-7）、repo（Task 8）、api（Task 9）共享。
- Consumes: `Money`（Task 4）。

- [ ] **Step 1: 写失败的账户状态机单测**

Create `templates/bank/internal/corebanking/domain/account_test.go`:
```go
package domain

import "testing"

func TestClose(t *testing.T) {
	got, err := Close(AccountStatusActive)
	if err != nil || got != AccountStatusClosed {
		t.Errorf("Close(active)=%q,%v, want closed", got, err)
	}
	if _, err := Close(AccountStatusFrozen); err == nil {
		t.Error("Close(frozen) 应报错")
	}
	if _, err := Close(AccountStatusClosed); err == nil {
		t.Error("Close(closed) 应报错（终态）")
	}
}

func TestFreezeUnfreeze(t *testing.T) {
	got, err := Freeze(AccountStatusActive)
	if err != nil || got != AccountStatusFrozen {
		t.Errorf("Freeze(active)=%q,%v, want frozen", got, err)
	}
	if _, err := Freeze(AccountStatusClosed); err == nil {
		t.Error("Freeze(closed) 应报错")
	}
	got, err = Unfreeze(AccountStatusFrozen)
	if err != nil || got != AccountStatusActive {
		t.Errorf("Unfreeze(frozen)=%q,%v, want active", got, err)
	}
	if _, err := Unfreeze(AccountStatusActive); err == nil {
		t.Error("Unfreeze(active) 应报错")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/domain/...
```
Expected: FAIL（`AccountStatus`、`Close` 未定义）。

- [ ] **Step 3: 写 account 实现 + 状态机**

Create `templates/bank/internal/corebanking/domain/account.go`:
```go
package domain

import "fmt"

// AccountStatus 账户状态。
type AccountStatus string

const (
	AccountStatusActive AccountStatus = "active"
	AccountStatusClosed AccountStatus = "closed"
	AccountStatusFrozen AccountStatus = "frozen"
)

// 账户状态机（合法迁移）：
//   active --Close--> closed（终态）
//   active --Freeze--> frozen
//   frozen --Unfreeze--> active

// Close 销户：仅 active → closed。
func Close(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusActive {
		return AccountStatusClosed, nil
	}
	return from, fmt.Errorf("account: 只有 active 可销户，当前 %q", from)
}

// Freeze 冻结：仅 active → frozen。
func Freeze(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusActive {
		return AccountStatusFrozen, nil
	}
	return from, fmt.Errorf("account: 只有 active 可冻结，当前 %q", from)
}

// Unfreeze 解冻：仅 frozen → active。
func Unfreeze(from AccountStatus) (AccountStatus, error) {
	if from == AccountStatusFrozen {
		return AccountStatusActive, nil
	}
	return from, fmt.Errorf("account: 只有 frozen 可解冻，当前 %q", from)
}

// DemandAccount 活期账户（对应 demand_account 表）。
type DemandAccount struct {
	AccountNo   string
	CustID      string
	Ccy         string
	Status      AccountStatus
	OpenBizDate string
	BranchCode  string
	ProductCode string
	SubjectCode string
}

// FixedAccount 定期账户（对应 fixed_account 表）。Principal 为金额（分），
// Rate 为 NUMERIC(10,6) 字符串（禁 float 运算，透传）。
type FixedAccount struct {
	AccountNo    string
	CustID       string
	Ccy          string
	Principal    Money
	Rate         string
	TermMonths   int
	StartBizDate string
	MatureDate   string
	Status       AccountStatus
	SubjectCode  string
}
```

- [ ] **Step 4: 写 ledger.go（科目/借贷/Entry/总账）**

Create `templates/bank/internal/corebanking/domain/ledger.go`:
```go
package domain

// DCFlag 借贷标志（复式记账核心）。
type DCFlag string

const (
	DCDebit  DCFlag = "借" // 资产、费用增加
	DCCredit DCFlag = "贷" // 负债、收入增加
)

// Subject 会计科目（对应 chart_of_acct 表）。
type Subject struct {
	Code       string
	Name       string
	DCAttr     DCFlag // 科目借贷属性
	Level      int
	ParentCode string
	Status     string
}

// Entry 一笔复式记账分录：哪个账户、借或贷、多少、入哪个科目。
type Entry struct {
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	SubjectCode string
}

// GLBalance 总账余额（对应 gl_balance 表）。
type GLBalance struct {
	SubjectCode string
	BizDate     string
	DCBalance   Money // 借方余额
	CCBalance   Money // 贷方余额
	Ccy         string
}

// BalanceDelta 某账户在某 biz_date 的余额增量（贷为正、借为负）。
// 过账时由 service 计算并交给 repo 累加。
type BalanceDelta struct {
	AccountNo   string
	Delta       Money
	SubjectCode string
}
```

- [ ] **Step 5: 写 txn.go（流水/余额）**

Create `templates/bank/internal/corebanking/domain/txn.go`:
```go
package domain

// Txn 账务流水（对应 acct_txn 表）。
type Txn struct {
	TxnID       string
	BizDate     string
	TxnTs       string // timestamp 文本
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	Ccy         string
	SubjectCode string
	OppAccount  string
	RefTxnID    string
	Channel     string
	Summary     string
}

// Balance 分户账余额快照（对应 account_balance 表）。
type Balance struct {
	AccountNo        string
	BizDate          string
	Balance          Money
	AvailableBalance Money
	FrozenAmount     Money
	SubjectCode      string
}
```

- [ ] **Step 6: 运行全部 domain 测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/domain/...
```
Expected: PASS（money + account 状态机全绿）。

- [ ] **Step 7: 提交**

```bash
cd .
git add templates/bank/internal/corebanking/domain
git commit -m "feat(bank): add account/ledger/txn domain types + state machine

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 6: service 层 — 复式记账 Post（借必等于贷）

**Files:**
- Create: `templates/bank/internal/corebanking/service/ledger_service.go`
- Create: `templates/bank/internal/corebanking/service/ledger_service_test.go`

**Interfaces:**
- Consumes: `domain.Entry/Money/Txn/GLBalance/DCFlag`（Task 4/5）。
- Produces: `service.ValidateBalance(entries) (debit,credit,err)`（纯函数，不平返回 `ErrUnbalanced`）；`service.LedgerService.Post(ctx, entries, bizDate, ccy) error`；`service.LedgerStore` 接口（`InsertTxns/ApplyBalanceDeltas/UpsertGL`，repo 在 Task 8 实现）；`service.BalanceDelta`。**`Post` 不平时拒绝、绝不调用 store**（验收 #7）。
- 后续 Task 8 的 repo 必须实现 `LedgerStore` 的这三个方法，签名逐字如下。

- [ ] **Step 1: 写失败的复式记账单测**

Create `templates/bank/internal/corebanking/service/ledger_service_test.go`:
```go
package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
)

func TestValidateBalance_Balanced(t *testing.T) {
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	debit, credit, err := ValidateBalance(entries)
	if err != nil {
		t.Fatalf("平衡应无错: %v", err)
	}
	if debit != 10000 || credit != 10000 {
		t.Errorf("debit=%d credit=%d, want 10000/10000", debit, credit)
	}
}

func TestValidateBalance_Unbalanced(t *testing.T) {
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 9999},
	}
	_, _, err := ValidateBalance(entries)
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("不平应返回 ErrUnbalanced, got %v", err)
	}
}

func TestPost_Unbalanced_RefusesAndDoesNotTouchStore(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 100},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 99},
	}
	err := svc.Post(context.Background(), entries, "2026-07-15", "CNY")
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post 不平应返回 ErrUnbalanced, got %v", err)
	}
	if store.calls != 0 {
		t.Errorf("不平时不应调用 store, 调用次数=%d", store.calls)
	}
}

func TestPost_Balanced_Persists(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	if err := svc.Post(context.Background(), entries, "2026-07-15", "CNY"); err != nil {
		t.Fatalf("Post 平账应成功: %v", err)
	}
	if len(store.txns) != 2 {
		t.Errorf("应写 2 笔流水, got %d", len(store.txns))
	}
	if len(store.deltas) != 2 {
		t.Errorf("应更新 2 个账户余额, got %d", len(store.deltas))
	}
	if store.gl == nil {
		t.Error("应更新总账")
	}
}

// recordingLedgerStore 记录调用，用于断言 Post 的副作用。
type recordingLedgerStore struct {
	calls  int
	txns   []domain.Txn
	deltas []domain.BalanceDelta
	gl     *domain.GLBalance
}

func (f *recordingLedgerStore) InsertTxns(_ context.Context, txns []domain.Txn) error {
	f.calls++
	f.txns = append(f.txns, txns...)
	return nil
}
func (f *recordingLedgerStore) ApplyBalanceDeltas(_ context.Context, _ string, deltas []domain.BalanceDelta) error {
	f.calls++
	f.deltas = append(f.deltas, deltas...)
	return nil
}
func (f *recordingLedgerStore) UpsertGL(_ context.Context, gl domain.GLBalance) error {
	f.calls++
	f.gl = &gl
	return nil
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/service/...
```
Expected: FAIL（`ValidateBalance`、`LedgerService`、`BalanceDelta` 未定义）。

- [ ] **Step 3: 写 ledger_service 实现**

Create `templates/bank/internal/corebanking/service/ledger_service.go`:
```go
// Package service 是 core-banking 用例层：业务规则，纯逻辑可单测。
package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
)

// ErrUnbalanced 借贷不平——复式记账核心不变量被违反。
var ErrUnbalanced = fmt.Errorf("ledger: 借贷不平")

// LedgerStore service 依赖的持久化接口（依赖倒置：repo 实现它）。
type LedgerStore interface {
	InsertTxns(ctx context.Context, txns []domain.Txn) error
	ApplyBalanceDeltas(ctx context.Context, bizDate string, deltas []domain.BalanceDelta) error
	UpsertGL(ctx context.Context, gl domain.GLBalance) error
}

// LedgerService 复式记账用例。
type LedgerService struct {
	store LedgerStore
}

func NewLedgerService(store LedgerStore) *LedgerService {
	return &LedgerService{store: store}
}

// ValidateBalance 校验复式记账平衡，返回借/贷合计。不平返回 ErrUnbalanced。
// 纯函数，无副作用，是验收 #7 的核心。
func ValidateBalance(entries []domain.Entry) (debit, credit domain.Money, err error) {
	for _, e := range entries {
		switch e.DCFlag {
		case domain.DCDebit:
			debit = debit.Add(e.Amount)
		case domain.DCCredit:
			credit = credit.Add(e.Amount)
		default:
			return 0, 0, fmt.Errorf("ledger: 非法借贷标志 %q", e.DCFlag)
		}
	}
	if debit != credit {
		return debit, credit, fmt.Errorf("%w: 借=%s 贷=%s", ErrUnbalanced, debit, credit)
	}
	return debit, credit, nil
}

// Post 过账：校验平衡 → 写流水 → 累加分户账余额 → 更新总账。
// 不平则拒绝且绝不调用 store（验收 #7）。
func (s *LedgerService) Post(ctx context.Context, entries []domain.Entry, bizDate, ccy string) error {
	if _, _, err := ValidateBalance(entries); err != nil {
		return err
	}
	txns, deltas, gl := summarize(entries, bizDate, ccy)
	if err := s.store.InsertTxns(ctx, txns); err != nil {
		return fmt.Errorf("ledger: 写流水失败: %w", err)
	}
	if err := s.store.ApplyBalanceDeltas(ctx, bizDate, deltas); err != nil {
		return fmt.Errorf("ledger: 更新分户账失败: %w", err)
	}
	return s.store.UpsertGL(ctx, gl)
}

func summarize(entries []domain.Entry, bizDate, ccy string) ([]domain.Txn, []domain.BalanceDelta, domain.GLBalance) {
	txns := make([]domain.Txn, 0, len(entries))
	byAcct := map[string]domain.Money{}
	subjByAcct := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, e := range entries {
		txns = append(txns, domain.Txn{
			BizDate: bizDate, AccountNo: e.AccountNo, DCFlag: e.DCFlag,
			Amount: e.Amount, Ccy: ccy, SubjectCode: e.SubjectCode,
		})
		if e.DCFlag == domain.DCCredit {
			byAcct[e.AccountNo] = byAcct[e.AccountNo].Add(e.Amount)
			glCC = glCC.Add(e.Amount)
		} else {
			byAcct[e.AccountNo] = byAcct[e.AccountNo].Sub(e.Amount)
			glDC = glDC.Add(e.Amount)
		}
		subjByAcct[e.AccountNo] = e.SubjectCode
	}
	deltas := make([]domain.BalanceDelta, 0, len(byAcct))
	for acct, d := range byAcct {
		deltas = append(deltas, BalanceDelta{AccountNo: acct, Delta: d, SubjectCode: subjByAcct[acct]})
	}
	gl := domain.GLBalance{BizDate: bizDate, DCBalance: glDC, CCBalance: glCC, Ccy: ccy}
	if len(deltas) > 0 {
		gl.SubjectCode = deltas[0].SubjectCode // Spec A 单科目过账简化
	}
	return txns, deltas, gl
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/service/...
```
Expected: PASS（含 `TestPost_Unbalanced_RefusesAndDoesNotTouchStore`）。

- [ ] **Step 5: 提交**

```bash
cd .
git add templates/bank/internal/corebanking/service
git commit -m "feat(bank): add ledger service with double-entry balance invariant

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: service 层 — 账户开户/状态流转 + 流水查询

**Files:**
- Create: `templates/bank/internal/corebanking/service/account_service.go`
- Create: `templates/bank/internal/corebanking/service/account_service_test.go`
- Create: `templates/bank/internal/corebanking/service/txn_service.go`

**Interfaces:**
- Consumes: `domain.DemandAccount/FixedAccount/AccountStatus` + 状态机函数（Task 5）。
- Produces: `service.AccountStore`（`InsertDemand/InsertFixed/SetDemandStatus`，repo 实现）；`service.AccountService.OpenDemand/OpenFixed/Close/Freeze/Unfreeze`；`service.TxnStore`（`ListTxns/GetLatestBalance`，repo 实现）；`service.TxnService.ListTxns/GetBalance`。Task 8 的 repo 必须实现这两个接口，签名逐字如下。

- [ ] **Step 1: 写失败的账户服务单测**

Create `templates/bank/internal/corebanking/service/account_service_test.go`:
```go
package service

import (
	"context"
	"testing"

	"bank/internal/corebanking/domain"
)

func TestOpenDemand_RejectsNonActive(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	err := svc.OpenDemand(context.Background(), domain.DemandAccount{
		AccountNo: "D1", Status: domain.AccountStatusFrozen,
	})
	if err == nil {
		t.Error("非 active 开户应报错")
	}
	if store.inserted != 0 {
		t.Error("非 active 不应落库")
	}
}

func TestOpenDemand_DefaultsActive(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	if err := svc.OpenDemand(context.Background(), domain.DemandAccount{AccountNo: "D1"}); err != nil {
		t.Fatalf("默认 active 开户应成功: %v", err)
	}
	if store.inserted != 1 || store.last.Status != domain.AccountStatusActive {
		t.Errorf("应落库 active 账户, inserted=%d last=%v", store.inserted, store.last.Status)
	}
}

func TestClose_EnforcesStateMachine(t *testing.T) {
	store := &recordingAccountStore{}
	svc := NewAccountService(store)
	// frozen 不能直接销户：状态机应拒绝
	if err := svc.Close(context.Background(), "D1", domain.AccountStatusFrozen); err == nil {
		t.Error("frozen 状态不应能直接销户")
	}
	if store.lastStatus == domain.AccountStatusClosed {
		t.Error("非法迁移不应落库")
	}
	if err := svc.Close(context.Background(), "D1", domain.AccountStatusActive); err != nil {
		t.Fatalf("active 销户应成功: %v", err)
	}
	if store.lastStatus != domain.AccountStatusClosed {
		t.Errorf("应置为 closed, got %q", store.lastStatus)
	}
}

type recordingAccountStore struct {
	inserted   int
	last       domain.DemandAccount
	lastStatus domain.AccountStatus
}

func (r *recordingAccountStore) InsertDemand(_ context.Context, a domain.DemandAccount) error {
	r.inserted++
	r.last = a
	return nil
}
func (r *recordingAccountStore) InsertFixed(_ context.Context, _ domain.FixedAccount) error {
	return nil
}
func (r *recordingAccountStore) SetDemandStatus(_ context.Context, _ string, s domain.AccountStatus) error {
	r.lastStatus = s
	return nil
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/service/...
```
Expected: FAIL（`AccountService`、`AccountStore` 未定义）。

- [ ] **Step 3: 写 account_service 实现**

Create `templates/bank/internal/corebanking/service/account_service.go`:
```go
package service

import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
)

// AccountStore 账户用例的持久化接口（repo 实现）。
type AccountStore interface {
	InsertDemand(ctx context.Context, a domain.DemandAccount) error
	InsertFixed(ctx context.Context, a domain.FixedAccount) error
	SetDemandStatus(ctx context.Context, accountNo string, status domain.AccountStatus) error
}

type AccountService struct {
	store AccountStore
}

func NewAccountService(store AccountStore) *AccountService {
	return &AccountService{store: store}
}

// OpenDemand 开活期账户。未指定状态时默认 active；强制新开户为 active。
func (s *AccountService) OpenDemand(ctx context.Context, a domain.DemandAccount) error {
	if a.Status == "" {
		a.Status = domain.AccountStatusActive
	}
	if a.Status != domain.AccountStatusActive {
		return fmt.Errorf("account: 新开户必须 active, got %q", a.Status)
	}
	return s.store.InsertDemand(ctx, a)
}

// OpenFixed 开定期账户（同样默认 active）。
func (s *AccountService) OpenFixed(ctx context.Context, a domain.FixedAccount) error {
	if a.Status == "" {
		a.Status = domain.AccountStatusActive
	}
	return s.store.InsertFixed(ctx, a)
}

// Close 销户：经状态机校验 active→closed。
func (s *AccountService) Close(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Close(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}

// Freeze 冻结：active→frozen。
func (s *AccountService) Freeze(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Freeze(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}

// Unfreeze 解冻：frozen→active。
func (s *AccountService) Unfreeze(ctx context.Context, accountNo string, current domain.AccountStatus) error {
	next, err := domain.Unfreeze(current)
	if err != nil {
		return err
	}
	return s.store.SetDemandStatus(ctx, accountNo, next)
}
```

- [ ] **Step 4: 写 txn_service（查询用例）**

Create `templates/bank/internal/corebanking/service/txn_service.go`:
```go
package service

import (
	"context"

	"bank/internal/corebanking/domain"
)

// TxnStore 流水/余额查询接口（只读，repo 实现）。
type TxnStore interface {
	ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error)
	GetLatestBalance(ctx context.Context, accountNo string) (domain.Balance, error)
}

type TxnService struct {
	store TxnStore
}

func NewTxnService(store TxnStore) *TxnService {
	return &TxnService{store: store}
}

// ListTxns 查流水（from/to 为 YYYY-MM-DD，空表示不限）。
func (s *TxnService) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	return s.store.ListTxns(ctx, accountNo, from, to)
}

// GetBalance 取最新 biz_date 的账户余额。
func (s *TxnService) GetBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	return s.store.GetLatestBalance(ctx, accountNo)
}
```

- [ ] **Step 5: 运行全部 service 测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/service/...
```
Expected: PASS（ledger + account 全绿）。

- [ ] **Step 6: 提交**

```bash
cd .
git add templates/bank/internal/corebanking/service
git commit -m "feat(bank): add account + txn services

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 8: repo 层 — pgx raw SQL 落库 + 分↔NUMERIC 转换

**Files:**
- Create: `templates/bank/internal/corebanking/repo/account_repo.go`
- Create: `templates/bank/internal/corebanking/repo/txn_repo.go`
- Create: `templates/bank/internal/corebanking/repo/ledger_repo.go`

**Interfaces:**
- Consumes: 全部 domain 类型（Task 4/5）+ service 定义的三个仓储接口（Task 6/7）。
- Produces: `repo.AccountRepo`（实现 `service.AccountStore` 的 `InsertDemand/InsertFixed/SetDemandStatus` + 查询 `GetDemand/GetFixed`）；`repo.TxnRepo`（实现 `service.TxnStore` 的 `ListTxns/GetLatestBalance`）；`repo.LedgerRepo`（实现 `service.LedgerStore` 的 `InsertTxns/ApplyBalanceDeltas/UpsertGL` + 查询 `GetGL`）。
- 规则：写入金额用 `Money.String()`（`$N` 参数传字符串到 `NUMERIC` 列）；读取金额用 `Scan` 到字符串再 `domain.ParseCents`（**全程无 float**）。空可选字段用 `nullable()` 转 `nil`（存 NULL）。
- repo **不 import service**（仅 import domain）；通过 Go 隐式接口实现 service 的仓储接口。Task 10 集成测试验证对真 Postgres 的读写。

- [ ] **Step 1: 写 account_repo**

Create `templates/bank/internal/corebanking/repo/account_repo.go`:
```go
// Package repo 是 core-banking 仓储层：pgx raw SQL 落库。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/corebanking/domain"
)

// AccountRepo 账户仓储。实现 service.AccountStore（写）+ 只读查询。
type AccountRepo struct {
	db *sql.DB
}

func NewAccountRepo(db *sql.DB) *AccountRepo { return &AccountRepo{db: db} }

// InsertDemand 实现 service.AccountStore.InsertDemand。
func (r *AccountRepo) InsertDemand(ctx context.Context, a domain.DemandAccount) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO demand_account
		(account_no,cust_id,ccy,acct_status,open_biz_date,branch_code,product_code,subject_code)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		a.AccountNo, a.CustID, a.Ccy, string(a.Status), a.OpenBizDate,
		a.BranchCode, a.ProductCode, a.SubjectCode)
	if err != nil {
		return fmt.Errorf("repo: 插入活期账户: %w", err)
	}
	return nil
}

// InsertFixed 实现 service.AccountStore.InsertFixed。
func (r *AccountRepo) InsertFixed(ctx context.Context, a domain.FixedAccount) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO fixed_account
		(account_no,cust_id,ccy,principal,rate,term_months,start_biz_date,mature_date,acct_status,subject_code)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		a.AccountNo, a.CustID, a.Ccy, a.Principal.String(), a.Rate, a.TermMonths,
		a.StartBizDate, a.MatureDate, string(a.Status), a.SubjectCode)
	if err != nil {
		return fmt.Errorf("repo: 插入定期账户: %w", err)
	}
	return nil
}

// SetDemandStatus 实现 service.AccountStore.SetDemandStatus。
func (r *AccountRepo) SetDemandStatus(ctx context.Context, accountNo string, status domain.AccountStatus) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE demand_account SET acct_status=$2 WHERE account_no=$1`, accountNo, string(status))
	if err != nil {
		return fmt.Errorf("repo: 更新账户状态: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("repo: 账户 %s 不存在", accountNo)
	}
	return nil
}

// GetDemand 查活期账户。不存在时返回包装的 sql.ErrNoRows。
func (r *AccountRepo) GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error) {
	row := r.db.QueryRowContext(ctx, `SELECT account_no,cust_id,ccy,acct_status,open_biz_date,
		branch_code,product_code,subject_code FROM demand_account WHERE account_no=$1`, accountNo)
	var a domain.DemandAccount
	var status string
	err := row.Scan(&a.AccountNo, &a.CustID, &a.Ccy, &status, &a.OpenBizDate,
		&a.BranchCode, &a.ProductCode, &a.SubjectCode)
	if err != nil {
		return domain.DemandAccount{}, fmt.Errorf("repo: 查活期账户 %s: %w", accountNo, err)
	}
	a.Status = domain.AccountStatus(status)
	return a, nil
}

// GetFixed 查定期账户。
func (r *AccountRepo) GetFixed(ctx context.Context, accountNo string) (domain.FixedAccount, error) {
	row := r.db.QueryRowContext(ctx, `SELECT account_no,cust_id,ccy,principal,rate,term_months,
		start_biz_date,mature_date,acct_status,subject_code FROM fixed_account WHERE account_no=$1`, accountNo)
	var (
		a           domain.FixedAccount
		status      string
		principalStr string
		rateStr     string
	)
	err := row.Scan(&a.AccountNo, &a.CustID, &a.Ccy, &principalStr, &rateStr, &a.TermMonths,
		&a.StartBizDate, &a.MatureDate, &status, &a.SubjectCode)
	if err != nil {
		return domain.FixedAccount{}, fmt.Errorf("repo: 查定期账户 %s: %w", accountNo, err)
	}
	p, err := domain.ParseCents(principalStr)
	if err != nil {
		return domain.FixedAccount{}, err
	}
	a.Principal = p
	a.Rate = rateStr
	a.Status = domain.AccountStatus(status)
	return a, nil
}
```

- [ ] **Step 2: 写 txn_repo**

Create `templates/bank/internal/corebanking/repo/txn_repo.go`:
```go
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/corebanking/domain"
)

// TxnRepo 流水/余额仓储。实现 service.TxnStore。
type TxnRepo struct {
	db *sql.DB
}

func NewTxnRepo(db *sql.DB) *TxnRepo { return &TxnRepo{db: db} }

// ListTxns 实现 service.TxnStore.ListTxns（from/to 为 YYYY-MM-DD，空表示不限）。
func (r *TxnRepo) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	q := `SELECT txn_id,biz_date::text,txn_ts::text,account_no,dc_flag,amount::text,ccy,subject_code,
		COALESCE(opp_account,''),COALESCE(ref_txn_id,''),COALESCE(channel,''),COALESCE(summary,'')
		FROM acct_txn WHERE 1=1`
	args := []any{}
	if accountNo != "" {
		args = append(args, accountNo)
		q += fmt.Sprintf(" AND account_no=$%d", len(args))
	}
	if from != "" {
		args = append(args, from)
		q += fmt.Sprintf(" AND biz_date>=$%d", len(args))
	}
	if to != "" {
		args = append(args, to)
		q += fmt.Sprintf(" AND biz_date<=$%d", len(args))
	}
	q += " ORDER BY txn_ts DESC LIMIT 200"
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repo: 查流水: %w", err)
	}
	defer rows.Close()
	var out []domain.Txn
	for rows.Next() {
		var t domain.Txn
		var amountStr string
		if err := rows.Scan(&t.TxnID, &t.BizDate, &t.TxnTs, &t.AccountNo, &t.DCFlag,
			&amountStr, &t.Ccy, &t.SubjectCode, &t.OppAccount, &t.RefTxnID,
			&t.Channel, &t.Summary); err != nil {
			return nil, fmt.Errorf("repo: 扫描流水: %w", err)
		}
		amt, err := domain.ParseCents(amountStr)
		if err != nil {
			return nil, err
		}
		t.Amount = amt
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetLatestBalance 实现 service.TxnStore.GetLatestBalance（取最新 biz_date 快照）。
func (r *TxnRepo) GetLatestBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	row := r.db.QueryRowContext(ctx, `SELECT account_no,biz_date::text,balance::text,available_balance::text,
		frozen_amount::text,subject_code FROM account_balance
		WHERE account_no=$1 ORDER BY biz_date DESC LIMIT 1`, accountNo)
	var (
		b         domain.Balance
		balStr    string
		availStr  string
		frozenStr string
	)
	err := row.Scan(&b.AccountNo, &b.BizDate, &balStr, &availStr, &frozenStr, &b.SubjectCode)
	if err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 查余额 %s: %w", accountNo, err)
	}
	if b.Balance, err = domain.ParseCents(balStr); err != nil {
		return domain.Balance{}, err
	}
	if b.AvailableBalance, err = domain.ParseCents(availStr); err != nil {
		return domain.Balance{}, err
	}
	if b.FrozenAmount, err = domain.ParseCents(frozenStr); err != nil {
		return domain.Balance{}, err
	}
	return b, nil
}
```

- [ ] **Step 3: 写 ledger_repo（含复式记账持久化 + 总账查询）**

Create `templates/bank/internal/corebanking/repo/ledger_repo.go`:
```go
package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"bank/internal/corebanking/domain"
)

// LedgerRepo 总账/复式记账仓储。实现 service.LedgerStore（写）+ 只读 GetGL。
type LedgerRepo struct {
	db *sql.DB
}

func NewLedgerRepo(db *sql.DB) *LedgerRepo { return &LedgerRepo{db: db} }

// InsertTxns 实现 service.LedgerStore.InsertTxns。
func (r *LedgerRepo) InsertTxns(ctx context.Context, txns []domain.Txn) error {
	for _, t := range txns {
		id := t.TxnID
		if id == "" {
			id = newTxnID()
		}
		_, err := r.db.ExecContext(ctx, `INSERT INTO acct_txn
			(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			id, t.BizDate, t.AccountNo, string(t.DCFlag), t.Amount.String(), t.Ccy, t.SubjectCode,
			nullable(t.OppAccount), nullable(t.RefTxnID), nullable(t.Channel), nullable(t.Summary))
		if err != nil {
			return fmt.Errorf("repo: 插入流水: %w", err)
		}
	}
	return nil
}

// ApplyBalanceDeltas 实现 service.LedgerStore.ApplyBalanceDeltas（ON CONFLICT 累加，无需读旧值）。
func (r *LedgerRepo) ApplyBalanceDeltas(ctx context.Context, bizDate string, deltas []domain.BalanceDelta) error {
	for _, d := range deltas {
		_, err := r.db.ExecContext(ctx, `INSERT INTO account_balance
			(account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
			VALUES ($1,$2,$3,$3,0,$4)
			ON CONFLICT (account_no,biz_date) DO UPDATE
			SET balance=account_balance.balance+EXCLUDED.balance,
			    available_balance=account_balance.available_balance+EXCLUDED.available_balance`,
			d.AccountNo, bizDate, d.Delta.String(), d.SubjectCode)
		if err != nil {
			return fmt.Errorf("repo: 累加余额: %w", err)
		}
	}
	return nil
}

// UpsertGL 实现 service.LedgerStore.UpsertGL（总账累加）。
func (r *LedgerRepo) UpsertGL(ctx context.Context, gl domain.GLBalance) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO gl_balance
		(subject_code,biz_date,dc_balance,cc_balance,ccy)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (subject_code,biz_date,ccy) DO UPDATE
		SET dc_balance=gl_balance.dc_balance+EXCLUDED.dc_balance,
		    cc_balance=gl_balance.cc_balance+EXCLUDED.cc_balance`,
		gl.SubjectCode, gl.BizDate, gl.DCBalance.String(), gl.CCBalance.String(), gl.Ccy)
	if err != nil {
		return fmt.Errorf("repo: 更新总账: %w", err)
	}
	return nil
}

// GetGL 查某 biz_date 的总账（API /ledger 用）。
func (r *LedgerRepo) GetGL(ctx context.Context, bizDate string) ([]domain.GLBalance, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT subject_code,biz_date::text,dc_balance::text,cc_balance::text,ccy
		 FROM gl_balance WHERE biz_date=$1 ORDER BY subject_code`, bizDate)
	if err != nil {
		return nil, fmt.Errorf("repo: 查总账: %w", err)
	}
	defer rows.Close()
	var out []domain.GLBalance
	for rows.Next() {
		var g domain.GLBalance
		var dcStr, ccStr string
		if err := rows.Scan(&g.SubjectCode, &g.BizDate, &dcStr, &ccStr, &g.Ccy); err != nil {
			return nil, fmt.Errorf("repo: 扫描总账: %w", err)
		}
		if g.DCBalance, err = domain.ParseCents(dcStr); err != nil {
			return nil, err
		}
		if g.CCBalance, err = domain.ParseCents(ccStr); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func newTxnID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "T" + hex.EncodeToString(b)
}
```

- [ ] **Step 4: 验证编译（repo 实现了 service 接口，编译即静态校验签名一致）**

Run:
```bash
cd ./templates/bank
go build ./...
```
Expected: 编译通过。若报接口未实现，对照 Task 6/7 的接口签名修正（签名必须逐字一致）。

- [ ] **Step 5: 提交**

```bash
cd .
git add templates/bank/internal/corebanking/repo
git commit -m "feat(bank): add account/txn/ledger repos with cents<->numeric conversion

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 9: api 层 — 只读 HTTP handlers + chi router

**Files:**
- Create: `templates/bank/internal/corebanking/api/handlers.go`
- Create: `templates/bank/internal/corebanking/api/router.go`
- Create: `templates/bank/internal/corebanking/api/handlers_test.go`

**Interfaces:**
- Consumes: domain 类型 + `service.TxnService`（Task 7）；定义 `AccountReader`/`LedgerReader` 只读接口，由 Task 8 的 `repo.AccountRepo`/`repo.LedgerRepo` 实现。
- Produces: `api.Handlers`（持有 `Accounts AccountReader`、`TxnSvc *service.TxnService`、`Ledger LedgerReader`）；`api.NewRouter(h *Handlers) http.Handler`。暴露 5 个只读端点（§7.4）：`GET /healthz`、`GET /api/v1/accounts/{account_no}`、`GET /api/v1/accounts/{account_no}/balance`、`GET /api/v1/txns`、`GET /api/v1/ledger`。
- 规则：金额一律 `Money.String()` 序列化为字符串（**无 float**）；不存在的资源 → 404；handler 依赖接口（可单测，用 httptest.NewServer 走完整路由）。

- [ ] **Step 1: 写 handlers + DTO + helper**

Create `templates/bank/internal/corebanking/api/handlers.go`:
```go
// Package api 是 core-banking 传输层：http handlers + chi router。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
)

// AccountReader 账户只读查询（repo.AccountRepo 实现）。
type AccountReader interface {
	GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error)
	GetFixed(ctx context.Context, accountNo string) (domain.FixedAccount, error)
}

// LedgerReader 总账只读查询（repo.LedgerRepo 实现）。
type LedgerReader interface {
	GetGL(ctx context.Context, bizDate string) ([]domain.GLBalance, error)
}

// Handlers 持有所有只读依赖。
type Handlers struct {
	Accounts AccountReader
	TxnSvc   *service.TxnService
	Ledger   LedgerReader
}

// Healthz 存活检查。
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GetAccount 查账户（先活期，无则定期，都无则 404）。
func (h *Handlers) GetAccount(w http.ResponseWriter, r *http.Request) {
	no := chiURLParam(r, "account_no")
	ctx := r.Context()
	if d, err := h.Accounts.GetDemand(ctx, no); err == nil {
		writeJSON(w, http.StatusOK, accountResp{
			AccountNo: d.AccountNo, CustID: d.CustID, Type: "demand",
			Ccy: d.Ccy, Status: string(d.Status),
		})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	if f, err := h.Accounts.GetFixed(ctx, no); err == nil {
		writeJSON(w, http.StatusOK, accountResp{
			AccountNo: f.AccountNo, CustID: f.CustID, Type: "fixed", Ccy: f.Ccy,
			Status: string(f.Status), Principal: f.Principal.String(), Rate: f.Rate,
			Term: f.TermMonths, MatureDate: f.MatureDate,
		})
		return
	} else if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, errMap(errors.New("账户不存在")))
		return
	} else {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
}

// GetBalance 查最新 biz_date 余额。
func (h *Handlers) GetBalance(w http.ResponseWriter, r *http.Request) {
	no := chiURLParam(r, "account_no")
	b, err := h.TxnSvc.GetBalance(r.Context(), no)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("无余额记录")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	writeJSON(w, http.StatusOK, balanceResp{
		AccountNo: b.AccountNo, BizDate: b.BizDate, Balance: b.Balance.String(),
		Available: b.AvailableBalance.String(), Frozen: b.FrozenAmount.String(),
	})
}

// ListTxns 查流水（query: account_no/from/to）。
func (h *Handlers) ListTxns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	txns, err := h.TxnSvc.ListTxns(r.Context(), q.Get("account_no"), q.Get("from"), q.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	out := make([]txnResp, 0, len(txns))
	for _, t := range txns {
		out = append(out, txnResp{
			TxnID: t.TxnID, BizDate: t.BizDate, AccountNo: t.AccountNo,
			DCFlag: string(t.DCFlag), Amount: t.Amount.String(), Summary: t.Summary,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"txns": out})
}

// GetLedger 查总账（query: biz_date）。
func (h *Handlers) GetLedger(w http.ResponseWriter, r *http.Request) {
	bizDate := r.URL.Query().Get("biz_date")
	if bizDate == "" {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("缺少 biz_date"))); return
	}
	gls, err := h.Ledger.GetGL(r.Context(), bizDate)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	out := make([]ledgerResp, 0, len(gls))
	for _, g := range gls {
		out = append(out, ledgerResp{
			SubjectCode: g.SubjectCode, BizDate: g.BizDate,
			DC: g.DCBalance.String(), CC: g.CCBalance.String(), Ccy: g.Ccy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ledger": out})
}

// --- DTO ---

type accountResp struct {
	AccountNo  string `json:"account_no"`
	CustID     string `json:"cust_id"`
	Type       string `json:"type"`
	Ccy        string `json:"ccy"`
	Status     string `json:"status"`
	Principal  string `json:"principal,omitempty"`
	Rate       string `json:"rate,omitempty"`
	Term       int    `json:"term_months,omitempty"`
	MatureDate string `json:"mature_date,omitempty"`
}

type balanceResp struct {
	AccountNo string `json:"account_no"`
	BizDate   string `json:"biz_date"`
	Balance   string `json:"balance"`
	Available string `json:"available"`
	Frozen    string `json:"frozen"`
}

type txnResp struct {
	TxnID     string `json:"txn_id"`
	BizDate   string `json:"biz_date"`
	AccountNo string `json:"account_no"`
	DCFlag    string `json:"dc_flag"`
	Amount    string `json:"amount"`
	Summary   string `json:"summary"`
}

type ledgerResp struct {
	SubjectCode string `json:"subject_code"`
	BizDate     string `json:"biz_date"`
	DC          string `json:"dc"`
	CC          string `json:"cc"`
	Ccy         string `json:"ccy"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string {
	return map[string]string{"error": err.Error()}
}
```

- [ ] **Step 2: 写 router（含 chiURLParam 适配）**

Create `templates/bank/internal/corebanking/api/router.go`:
```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter 装配只读路由。
func NewRouter(h *Handlers) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logger, middleware.Recoverer)
	r.Get("/healthz", h.Healthz)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/accounts/{account_no}", h.GetAccount)
		r.Get("/accounts/{account_no}/balance", h.GetBalance)
		r.Get("/txns", h.ListTxns)
		r.Get("/ledger", h.GetLedger)
	})
	return r
}

// chiURLParam 从 chi 路由上下文取路径参数。
func chiURLParam(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}
```

- [ ] **Step 3: 写 handler 单测（走完整路由）**

Create `templates/bank/internal/corebanking/api/handlers_test.go`:
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

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
)

type fakeAccounts struct {
	demand    *domain.DemandAccount
	fixed     *domain.FixedAccount
	demandErr error
}

func (f fakeAccounts) GetDemand(_ context.Context, _ string) (domain.DemandAccount, error) {
	if f.demandErr != nil {
		return domain.DemandAccount{}, f.demandErr
	}
	if f.demand != nil {
		return *f.demand, nil
	}
	return domain.DemandAccount{}, sql.ErrNoRows
}
func (f fakeAccounts) GetFixed(_ context.Context, _ string) (domain.FixedAccount, error) {
	if f.fixed != nil {
		return *f.fixed, nil
	}
	return domain.FixedAccount{}, sql.ErrNoRows
}

type fakeLedger struct{ gls []domain.GLBalance }

func (f fakeLedger) GetGL(context.Context, string) ([]domain.GLBalance, error) { return f.gls, nil }

type fakeTxnStore struct{ bal *domain.Balance }

func (f fakeTxnStore) ListTxns(context.Context, string, string, string) ([]domain.Txn, error) {
	return nil, nil
}
func (f fakeTxnStore) GetLatestBalance(context.Context, string) (domain.Balance, error) {
	if f.bal != nil {
		return *f.bal, nil
	}
	return domain.Balance{}, sql.ErrNoRows
}

func getBody(t *testing.T, h http.Handler, path string) (int, string) {
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
	code, body := getBody(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetAccount_Demand(t *testing.T) {
	h := &Handlers{Accounts: fakeAccounts{demand: &domain.DemandAccount{
		AccountNo: "D1", CustID: "C1", Ccy: "CNY", Status: domain.AccountStatusActive,
	}}}
	code, body := getBody(t, NewRouter(h), "/api/v1/accounts/D1")
	if code != 200 || !strings.Contains(body, `"cust_id":"C1"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetAccount_NotFound(t *testing.T) {
	h := &Handlers{Accounts: fakeAccounts{}}
	code, _ := getBody(t, NewRouter(h), "/api/v1/accounts/NOPE")
	if code != 404 {
		t.Errorf("want 404, got %d", code)
	}
}

func TestGetBalance(t *testing.T) {
	h := &Handlers{TxnSvc: service.NewTxnService(fakeTxnStore{bal: &domain.Balance{
		AccountNo: "D1", BizDate: "2026-07-15", Balance: domain.NewMoneyFromCents(123456),
		AvailableBalance: domain.NewMoneyFromCents(123456),
	}})}
	code, body := getBody(t, NewRouter(h), "/api/v1/accounts/D1/balance")
	if code != 200 || !strings.Contains(body, `"balance":"1234.56"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetLedger_MissingBizDate(t *testing.T) {
	h := &Handlers{Ledger: fakeLedger{}}
	code, _ := getBody(t, NewRouter(h), "/api/v1/ledger")
	if code != 400 {
		t.Errorf("缺少 biz_date 应 400, got %d", code)
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/corebanking/api/...
```
Expected: PASS（healthz/account/balance/ledger 全绿）。

- [ ] **Step 5: 提交**

```bash
cd .
git add templates/bank/internal/corebanking/api
git commit -m "feat(bank): add read-only http api with chi router

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 10: cmd/core-banking 入口 + repo/api 集成测试

**Files:**
- Create: `templates/bank/cmd/core-banking/main.go`
- Create: `templates/bank/internal/corebanking/repo/integration_test.go`

**Interfaces:**
- Consumes: 全部 service/repo/api + platform（Task 3/6/7/8/9）。
- Produces: 可运行的 `core-banking` 二进制（连 `core_db`，起只读 HTTP server，监听 `API_PORT`，启动时短暂重试连库）；集成测试（`//go:build integration`）验证 repo 对真 Postgres 的读写 + 复式记账累加语义。
- 集成测试需 Postgres 运行（`make up` 起 postgres）；无连接时 `t.Skip`。

- [ ] **Step 1: 写 core-banking 服务入口**

Create `templates/bank/cmd/core-banking/main.go`:
```go
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
```

- [ ] **Step 2: 写 repo 集成测试（build tag integration）**

Create `templates/bank/internal/corebanking/repo/integration_test.go`:
```go
//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/repo"
	"bank/internal/platform/pg"
)

func setupDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("core_db")
	if err != nil {
		t.Skipf("无 core_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up）: %v", err)
	}
	return db
}

func TestAccountRepo_InsertAndGetDemand(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)

	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no='IT-D1'")
	if err := ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: "IT-D1", CustID: "C1", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := ar.GetDemand(ctx, "IT-D1")
	if err != nil {
		t.Fatal(err)
	}
	if got.CustID != "C1" || got.Status != domain.AccountStatusActive {
		t.Errorf("got cust_id=%s status=%s", got.CustID, got.Status)
	}
}

func TestLedgerRepo_BalanceDelta_Accumulates(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)

	for _, no := range []string{"IT-D1", "IT-D2"} {
		db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", no)
		ar.InsertDemand(ctx, domain.DemandAccount{
			AccountNo: no, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: "2026-07-15", SubjectCode: "2011",
		})
	}
	deltas := []domain.BalanceDelta{
		{AccountNo: "IT-D1", Delta: domain.NewMoneyFromCents(10000), SubjectCode: "2011"},
		{AccountNo: "IT-D2", Delta: domain.NewMoneyFromCents(-10000), SubjectCode: "2011"},
	}
	if err := lr.ApplyBalanceDeltas(ctx, "2026-07-15", deltas); err != nil {
		t.Fatal(err)
	}
	// 重复应用应累加
	if err := lr.ApplyBalanceDeltas(ctx, "2026-07-15", deltas); err != nil {
		t.Fatal(err)
	}
	tr := repo.NewTxnRepo(db)
	b, err := tr.GetLatestBalance(ctx, "IT-D1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Balance != domain.NewMoneyFromCents(20000) {
		t.Errorf("累加后余额=%s, want 200.00", b.Balance)
	}
}
```

- [ ] **Step 3: 验证编译 + 单测**

Run:
```bash
cd ./templates/bank
go build ./...
go test ./...                 # 不含 integration（build tag 默认关闭）
go vet ./...
```
Expected: 编译通过；非集成测试全绿（domain/service/api）；`integration_test.go` 因 build tag 被排除。

- [ ] **Step 4: 验证集成测试（需先起 postgres）**

Run:
```bash
cd ./templates/bank
docker compose up -d postgres
# 等 postgres healthy 后（建库 schema 由 seed 完成；此处仅验证 repo 连接与表已存在）
# 注意：首次需先跑过一次 make seed 建好 core_db 与表
go test -tags=integration -run 'TestAccountRepo|TestLedgerRepo' ./internal/corebanking/repo/...
```
Expected: 若 postgres 已起且 core_db+表就绪 → PASS；否则 SKIP（不阻塞非集成 CI）。

- [ ] **Step 5: 提交**

```bash
cd .
git add templates/bank/cmd/core-banking templates/bank/internal/corebanking/repo/integration_test.go
git commit -m "feat(bank): add core-banking server entrypoint + integration tests

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 11: fixture 生成器 — config + 确定性 RNG

**Files:**
- Create: `templates/bank/internal/fixtures/config.go`
- Create: `templates/bank/internal/fixtures/config_test.go`
- Create: `templates/bank/internal/fixtures/rng.go`
- Create: `templates/bank/internal/fixtures/rng_test.go`

**Interfaces:**
- Produces: `fixtures.Scale`（`ScaleDev="dev"`/`ScaleFull="full"`）；`fixtures.Counts`；`fixtures.Config{StartBizDate,EndBizDate,Scale,Seed}`；`fixtures.DefaultConfig(scale) Config`；`Config.TargetCounts() Counts`；`fixtures.NewRNG(seed int64) *RNG`（确定性）；`RNG.IntRange(lo,hi int) int`（含两端）；`RNG.Choice([]string) string`；词库 `fixtures.Surnames/GivenNames/Branches/Channels/Summaries`。
- 目标量级（DEV≈FULL 的 1/4）：DEV {DemandAccounts:2000, FixedAccounts:500, DailyTxnLo:500, DailyTxnHi:1250}；FULL {DemandAccounts:8000, FixedAccounts:2000, DailyTxnLo:2000, DailyTxnHi:5000}。
- 确定性来源：`math/rand/v2` + `rand.NewPCG(seed,seed)`（Go 1.22）。同 `Seed` → 同序列。

- [ ] **Step 1: 写 config + 测试**

Create `templates/bank/internal/fixtures/config.go`:
```go
// Package fixtures 是 bank 工程的确定性 fixture 生成器。
package fixtures

// Scale 规模。
type Scale string

const (
	ScaleDev  Scale = "dev"
	ScaleFull Scale = "full"
)

// Counts 各实体的目标量级。
type Counts struct {
	Customers      int
	DemandAccounts int
	FixedAccounts  int
	DailyTxnLo     int
	DailyTxnHi     int
}

// DEV 约为 FULL 的 1/4。
var targetCounts = map[Scale]Counts{
	ScaleDev:  {Customers: 1250, DemandAccounts: 2000, FixedAccounts: 500, DailyTxnLo: 500, DailyTxnHi: 1250},
	ScaleFull: {Customers: 5000, DemandAccounts: 8000, FixedAccounts: 2000, DailyTxnLo: 2000, DailyTxnHi: 5000},
}

// Config fixture 配置。
type Config struct {
	StartBizDate string // YYYY-MM-DD
	EndBizDate   string
	Scale        Scale
	Seed         int64
}

// DefaultConfig 按规模给默认（start 2025-06-01, end 2026-07-13, seed 42）。
func DefaultConfig(scale Scale) Config {
	return Config{StartBizDate: "2025-06-01", EndBizDate: "2026-07-13", Scale: scale, Seed: 42}
}

// TargetCounts 返回当前规模的目标量级。
func (c Config) TargetCounts() Counts {
	if tc, ok := targetCounts[c.Scale]; ok {
		return tc
	}
	return targetCounts[ScaleDev]
}
```

Create `templates/bank/internal/fixtures/config_test.go`:
```go
package fixtures

import "testing"

func TestTargetCounts(t *testing.T) {
	dev := DefaultConfig(ScaleDev).TargetCounts()
	if dev.DemandAccounts != 2000 {
		t.Errorf("dev demand=%d, want 2000", dev.DemandAccounts)
	}
	full := DefaultConfig(ScaleFull).TargetCounts()
	if full.DemandAccounts != 8000 {
		t.Errorf("full demand=%d, want 8000", full.DemandAccounts)
	}
	if full.DemandAccounts != 4*dev.DemandAccounts {
		t.Errorf("FULL 应为 DEV 的 4 倍")
	}
}
```

- [ ] **Step 2: 写 rng + 确定性测试**

Create `templates/bank/internal/fixtures/rng.go`:
```go
package fixtures

import "math/rand/v2"

// RNG 确定性随机源。同 Seed → 同序列（可复现 + 单测哈希比对）。
// 用 math/rand/v2 的 PCG，定步长种子，零重依赖。
type RNG struct {
	r *rand.Rand
}

// NewRNG 用 seed 构造确定性 RNG。
func NewRNG(seed int64) *RNG {
	return &RNG{r: rand.New(rand.NewPCG(uint64(seed), uint64(seed)))}
}

// IntRange 返回 [lo, hi] 的随机整数（含两端）。
func (g *RNG) IntRange(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + g.r.IntN(hi-lo+1)
}

// Choice 从列表随机选一个。
func (g *RNG) Choice(list []string) string {
	return list[g.r.IntN(len(list))]
}

// 手写小词库（zh_CN 语义，零外部依赖）。
var (
	Surnames   = []string{"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "吴", "周"}
	GivenNames = []string{"伟", "芳", "娜", "秀英", "敏", "静", "磊", "强", "洋", "艳"}
	Branches   = []string{"HO", "SH", "BJ", "GZ", "CD"}
	Channels   = []string{"网银", "手机", "ATM", "柜面"}
	Summaries  = []string{"工资", "转账", "消费", "存款", "取款"}
)
```

Create `templates/bank/internal/fixtures/rng_test.go`:
```go
package fixtures

import "testing"

func TestRNG_Deterministic(t *testing.T) {
	g1 := NewRNG(42)
	g2 := NewRNG(42)
	for i := 0; i < 100; i++ {
		a, b := g1.IntRange(1, 1000), g2.IntRange(1, 1000)
		if a != b {
			t.Fatalf("第 %d 次: 同 seed 序列不一致 %d!=%d", i, a, b)
		}
	}
}

func TestRNG_DifferentSeedDiffers(t *testing.T) {
	if NewRNG(1).IntRange(0, 1<<30) == NewRNG(2).IntRange(0, 1<<30) {
		t.Error("不同 seed 应产生不同序列")
	}
}

func TestChoice(t *testing.T) {
	g := NewRNG(42)
	got := g.Choice(Branches)
	for _, b := range Branches {
		if b == got {
			return
		}
	}
	t.Errorf("Choice 返回 %q 不在词库", got)
}
```

- [ ] **Step 3: 运行测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/fixtures/...
```
Expected: PASS（config + rng 确定性）。

- [ ] **Step 4: 提交**

```bash
cd .
git add templates/bank/internal/fixtures
git commit -m "feat(bank): add fixture config + deterministic rng

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 12: fixture 生成器 — domains/core（静态主数据 + 账户 + 基线余额 + 流水）

**Files:**
- Create: `templates/bank/internal/fixtures/domains/core.go`
- Create: `templates/bank/internal/fixtures/domains/core_test.go`

**Interfaces:**
- Consumes: `fixtures.Config/Counts/NewRNG/词库`（Task 11）+ `domain.*`（Task 4/5）。
- Produces（纯生成器，确定性，验收 #6 的核心）：
  - `domains.GenStaticData(cfg) StaticData`
  - `domains.GenAccountRows(cfg) (demand []domain.DemandAccount, fixed []domain.FixedAccount)`
  - `domains.GenBalanceRows(cfg, demandNos []string) []domain.Balance`
  - `domains.GenTxnRows(cfg, demandNos []string) []domain.Txn`
- Produces（落库 writer，由 cmd/seed 调用）：`domains.WriteStatic/WriteAccounts/WriteBalances/WriteTxns`（幂等：先 DELETE 后 INSERT）。
- 纵切简化：core-banking **自包含**——`cust_id` 自生成（`C0000001`…），不依赖 `cust_db`/customers；基线 balance = 每个活期账户一条 `EndBizDate` 快照；少量近期流水（最近 5 天，每日量级缩小）——完整多日切日引擎属 Spec B。
- 确定性：所有随机用 `NewRNG(seed+offset)`，`account_no`/`txn_id` 用序号（非随机 uuid），同 `Config` 两次 `Gen*` 输出 `reflect.DeepEqual` 相等。

- [ ] **Step 1: 写生成器 + writer**

Create `templates/bank/internal/fixtures/domains/core.go`:
```go
// Package domains 是 fixture 的各业务域生成器。core = 核心账务。
package domains

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
)

// ---- 静态主数据（确定性）----

// StaticData 5 张主数据表的行集合。
type StaticData struct {
	SysParams [][2]string // {key, value}
	Ccys      [][4]string // {code, name, decimal_digits, status}
	Branches  [][5]string // {code, name, parent, region, level}
	Subjects  [][5]string // {code, name, dc_attr, level, parent}
	Rates     [][5]string // {rate_id, acct_type, ccy, rate_value, effective_date}
}

// GenStaticData 生成静态主数据（固定值 + cfg 的起始日）。
func GenStaticData(cfg fixtures.Config) StaticData {
	return StaticData{
		SysParams: [][2]string{
			{"biz_date", cfg.StartBizDate},
			{"prev_biz_date", cfg.StartBizDate},
			{"biz_status", "open"},
			{"last_cutover_ts", ""},
		},
		Ccys: [][4]string{
			{"CNY", "人民币", "2", "active"}, {"USD", "美元", "2", "active"},
			{"HKD", "港币", "2", "active"}, {"EUR", "欧元", "2", "active"},
		},
		Branches: [][5]string{
			{"HO", "总行", "", "华东", "1"}, {"SH", "上海分行", "HO", "华东", "2"},
			{"BJ", "北京分行", "HO", "华北", "2"}, {"GZ", "广州分行", "HO", "华南", "2"},
			{"CD", "成都分行", "HO", "西南", "2"}, {"SH-PD", "浦东支行", "SH", "华东", "3"},
			{"BJ-HD", "海淀支行", "BJ", "华北", "3"},
		},
		Subjects: [][5]string{
			{"1001", "库存现金", "借", "1", ""}, {"1002", "活期存款-资产", "借", "2", "1001"},
			{"2011", "活期存款", "贷", "2", ""}, {"2012", "定期存款", "贷", "2", ""},
			{"1301", "贷款", "借", "2", ""}, {"1311", "应收利息", "借", "2", ""},
			{"4001", "理财资金", "贷", "2", ""}, {"6011", "利息收入", "贷", "2", ""},
			{"6021", "手续费收入", "贷", "2", ""},
		},
		Rates: [][5]string{
			{"R-DMD-CNY", "demand", "CNY", "0.003000", cfg.StartBizDate},
			{"R-FIX3-CNY", "fixed_3m", "CNY", "0.012500", cfg.StartBizDate},
			{"R-FIX12-CNY", "fixed_12m", "CNY", "0.019000", cfg.StartBizDate},
			{"R-LOAN-CNY", "loan", "CNY", "0.043500", cfg.StartBizDate},
		},
	}
}

// GenAccountRows 生成活期/定期账户。cust_id 自生成（core-banking 自包含）。
func GenAccountRows(cfg fixtures.Config) ([]domain.DemandAccount, []domain.FixedAccount) {
	rng := fixtures.NewRNG(cfg.Seed + 1)
	tc := cfg.TargetCounts()
	nCustomers := tc.DemandAccounts / 2
	if nCustomers < 1 {
		nCustomers = 1
	}
	demand := make([]domain.DemandAccount, 0, tc.DemandAccounts)
	var fixed []domain.FixedAccount
	termRate := map[int]string{3: "0.012500", 6: "0.015000", 12: "0.019000"}
	terms := []int{3, 6, 12}
	for i := 0; i < tc.DemandAccounts; i++ {
		demand = append(demand, domain.DemandAccount{
			AccountNo: fmt.Sprintf("D%010d", i+1),
			CustID:    fmt.Sprintf("C%07d", (i%nCustomers)+1),
			Ccy:       "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: cfg.StartBizDate, BranchCode: rng.Choice(fixtures.Branches),
			ProductCode: "DEMAND-CNY", SubjectCode: "2011",
		})
	}
	// 定期：约 DemandAccounts/4 个
	nFixed := tc.DemandAccounts / 4
	for i := 0; i < nFixed; i++ {
		term := terms[rng.IntRange(0, 2)]
		fixed = append(fixed, domain.FixedAccount{
			AccountNo:    fmt.Sprintf("F%010d", i+1),
			CustID:       fmt.Sprintf("C%07d", (i%nCustomers)+1),
			Ccy:          "CNY",
			Principal:    domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 10000),
			Rate:         termRate[term],
			TermMonths:   term,
			StartBizDate: cfg.StartBizDate,
			MatureDate:   addMonths(cfg.StartBizDate, term),
			Status:       domain.AccountStatusActive, SubjectCode: "2012",
		})
	}
	return demand, fixed
}

// GenBalanceRows 为每个活期账户生成一条 EndBizDate 的基线余额快照。
func GenBalanceRows(cfg fixtures.Config, demandNos []string) []domain.Balance {
	rng := fixtures.NewRNG(cfg.Seed + 2)
	rows := make([]domain.Balance, 0, len(demandNos))
	for _, no := range demandNos {
		bal := domain.NewMoneyFromCents(int64(rng.IntRange(1, 9999)) * 100)
		rows = append(rows, domain.Balance{
			AccountNo: no, BizDate: cfg.EndBizDate,
			Balance: bal, AvailableBalance: bal, FrozenAmount: domain.NewMoneyFromCents(0),
			SubjectCode: "2011",
		})
	}
	return rows
}

// GenTxnRows 生成少量近期流水（最近 5 天，每日量级缩小）。
// 完整多日切日引擎属 Spec B。
func GenTxnRows(cfg fixtures.Config, demandNos []string) []domain.Txn {
	rng := fixtures.NewRNG(cfg.Seed + 3)
	tc := cfg.TargetCounts()
	days := recentDates(cfg.EndBizDate, 5)
	perDay := tc.DailyTxnLo / 100 // 缩影：dev 500/100=5 笔/天
	if perDay < 1 {
		perDay = 1
	}
	dc := []string{string(domain.DCCredit), string(domain.DCCredit), string(domain.DCDebit)} // 贷多借少
	var rows []domain.Txn
	seq := 0
	for _, d := range days {
		for i := 0; i < perDay; i++ {
			seq++
			rows = append(rows, domain.Txn{
				TxnID:       fmt.Sprintf("T%s-%06d", d, seq),
				BizDate:     d,
				AccountNo:   demandNos[rng.IntRange(0, len(demandNos)-1)],
				DCFlag:      domain.DCFlag(rng.Choice(dc)),
				Amount:      domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 10),
				Ccy:         "CNY", SubjectCode: "2011",
				OppAccount: demandNos[rng.IntRange(0, len(demandNos)-1)],
				Channel:    rng.Choice(fixtures.Channels),
				Summary:    rng.Choice(fixtures.Summaries),
			})
		}
	}
	return rows
}

// ---- 落库 writer（幂等：先 DELETE 后 INSERT）----

// WriteStatic 写 5 张主数据表。
func WriteStatic(ctx context.Context, db *sql.DB, data StaticData) error {
	for _, t := range []string{"sys_param", "ccy", "branch", "chart_of_acct", "interest_rate"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, p := range data.SysParams {
		if _, err := db.ExecContext(ctx, "INSERT INTO sys_param(param_key,param_value) VALUES($1,$2)", p[0], p[1]); err != nil {
			return err
		}
	}
	for _, c := range data.Ccys {
		if _, err := db.ExecContext(ctx, "INSERT INTO ccy(ccy_code,ccy_name,decimal_digits,status) VALUES($1,$2,$3,$4)", c[0], c[1], c[2], c[3]); err != nil {
			return err
		}
	}
	for _, b := range data.Branches {
		var parent any
		if b[2] != "" {
			parent = b[2]
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO branch(branch_code,branch_name,parent_branch,region,level,status) VALUES($1,$2,$3,$4,$5,'active')", b[0], b[1], parent, b[3], b[4]); err != nil {
			return err
		}
	}
	for _, s := range data.Subjects {
		var parent any
		if s[4] != "" {
			parent = s[4]
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO chart_of_acct(subject_code,subject_name,dc_attr,level,parent_subject,status) VALUES($1,$2,$3,$4,$5,'active')", s[0], s[1], s[2], s[3], s[4], parent); err != nil {
			return err
		}
	}
	for _, r := range data.Rates {
		if _, err := db.ExecContext(ctx, "INSERT INTO interest_rate(rate_id,acct_type,ccy,rate_value,effective_biz_date,status) VALUES($1,$2,$3,$4,$5,'active')", r[0], r[1], r[2], r[3], r[4]); err != nil {
			return err
		}
	}
	return nil
}

// WriteAccounts 写活期/定期账户（先清后插）。
func WriteAccounts(ctx context.Context, db *sql.DB, demand []domain.DemandAccount, fixed []domain.FixedAccount) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM demand_account"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM fixed_account"); err != nil {
		return err
	}
	for _, a := range demand {
		if _, err := db.ExecContext(ctx, `INSERT INTO demand_account
			(account_no,cust_id,ccy,acct_status,open_biz_date,branch_code,product_code,subject_code)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			a.AccountNo, a.CustID, a.Ccy, string(a.Status), a.OpenBizDate,
			a.BranchCode, a.ProductCode, a.SubjectCode); err != nil {
			return err
		}
	}
	for _, a := range fixed {
		if _, err := db.ExecContext(ctx, `INSERT INTO fixed_account
			(account_no,cust_id,ccy,principal,rate,term_months,start_biz_date,mature_date,acct_status,subject_code)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			a.AccountNo, a.CustID, a.Ccy, a.Principal.String(), a.Rate, a.TermMonths,
			a.StartBizDate, a.MatureDate, string(a.Status), a.SubjectCode); err != nil {
			return err
		}
	}
	return nil
}

// WriteBalances 写余额快照（先清后插）。
func WriteBalances(ctx context.Context, db *sql.DB, rows []domain.Balance) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM account_balance"); err != nil {
		return err
	}
	for _, b := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO account_balance
			(account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			b.AccountNo, b.BizDate, b.Balance.String(), b.AvailableBalance.String(),
			b.FrozenAmount.String(), b.SubjectCode); err != nil {
			return err
		}
	}
	return nil
}

// WriteTxns 写流水（先清后插）。
func WriteTxns(ctx context.Context, db *sql.DB, rows []domain.Txn) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM acct_txn"); err != nil {
		return err
	}
	for _, t := range rows {
		if _, err := db.ExecContext(ctx, `INSERT INTO acct_txn
			(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,channel,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			t.TxnID, t.BizDate, t.AccountNo, string(t.DCFlag), t.Amount.String(),
			t.Ccy, t.SubjectCode, nullable(t.OppAccount), nullable(t.Channel), nullable(t.Summary)); err != nil {
			return err
		}
	}
	return nil
}

// ---- helpers ----

func addMonths(dateStr string, months int) string {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return dateStr
	}
	return t.AddDate(0, months, 0).Format("2006-01-02")
}

func recentDates(endStr string, n int) []string {
	t, err := time.Parse("2006-01-02", endStr)
	if err != nil {
		return []string{endStr}
	}
	out := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, t.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

- [ ] **Step 2: 写确定性单测**

Create `templates/bank/internal/fixtures/domains/core_test.go`:
```go
package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenAccountRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	d1, f1 := GenAccountRows(cfg)
	d2, f2 := GenAccountRows(cfg)
	if !reflect.DeepEqual(d1, d2) || !reflect.DeepEqual(f1, f2) {
		t.Fatal("同 Config 两次 GenAccountRows 不一致（违反确定性）")
	}
	if len(d1) == 0 {
		t.Error("应生成活期账户")
	}
}

func TestGenBalanceRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	demand, _ := GenAccountRows(cfg)
	nos := []string{demand[0].AccountNo, demand[1].AccountNo}
	r1 := GenBalanceRows(cfg, nos)
	r2 := GenBalanceRows(cfg, nos)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("GenBalanceRows 不确定")
	}
}

func TestGenTxnRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	demand, _ := GenAccountRows(cfg)
	nos := make([]string, 10)
	for i := 0; i < 10; i++ {
		nos[i] = demand[i].AccountNo
	}
	r1 := GenTxnRows(cfg, nos)
	r2 := GenTxnRows(cfg, nos)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("GenTxnRows 不确定")
	}
	// 每条流水应有确定性 txn_id（非随机 uuid）
	for _, tx := range r1 {
		if tx.TxnID == "" {
			t.Error("txn_id 不应为空")
		}
	}
}

func TestGenStaticData_FixedContent(t *testing.T) {
	d := GenStaticData(fixtures.DefaultConfig(fixtures.ScaleDev))
	if len(d.Ccys) != 4 || len(d.Branches) != 7 || len(d.Subjects) != 9 || len(d.Rates) != 4 {
		t.Errorf("静态数据量级不符: ccy=%d branch=%d subj=%d rate=%d",
			len(d.Ccys), len(d.Branches), len(d.Subjects), len(d.Rates))
	}
}
```

- [ ] **Step 3: 运行测试确认通过**

Run:
```bash
cd ./templates/bank
go test ./internal/fixtures/...
```
Expected: PASS（config/rng/domains 全绿；确定性是验收 #6 的基础）。

- [ ] **Step 4: 提交**

```bash
cd .
git add templates/bank/internal/fixtures/domains
git commit -m "feat(bank): add deterministic core fixtures (static/accounts/balance/txn)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 13: cmd/seed 入口 + bank 模板本地端到端冒烟

**Files:**
- Create: `templates/bank/cmd/seed/main.go`

**Interfaces:**
- Consumes: `fixtures.DefaultConfig/Scale`（Task 11）+ `domains.Gen*/Write*`（Task 12）+ `pg.Open`（Task 3）+ `migrate.Run`（Task 3）。
- Produces: 可运行的 `seed` 二进制。编排 4 步：①建库（`--reset` 时 DROP+CREATE）→ ②连 `core_db` 跑 `core_db.sql` → ③生成 fixture → ④落库。连不上 postgres 时短暂重试后透出错误并提示"先 make up"。
- 设计文档 §7.3：seed 负责 建库 → 建表 → 灌数据，幂等（`--reset` 先 DROP 再建）。

- [ ] **Step 1: 写 seed 入口**

Create `templates/bank/cmd/seed/main.go`:
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
```

- [ ] **Step 2: 编译 + 单测全绿（bank 模板）**

Run:
```bash
cd ./templates/bank
go build ./...
go test ./...
go vet ./...
```
Expected: bank module 整体编译通过；全部非集成测试 PASS（domain/service/api/fixtures）。

- [ ] **Step 3: bank 模板本地端到端冒烟（需 docker）**

Run:
```bash
cd ./templates/bank
docker compose up -d postgres
# 等 postgres healthy
go run ./cmd/seed --scale=dev --reset
go run ./cmd/core-banking &  # 或 docker compose up -d core-banking
sleep 2
curl -sf localhost:8080/healthz && echo
curl -sf localhost:8080/api/v1/accounts/D0000000001 && echo
curl -sf localhost:8080/api/v1/accounts/D0000000001/balance && echo
# 清理（可选）
# docker compose down
```
Expected: seed 打印"完成 ✅ 活期 2000…"；`/healthz` 返回 `{"status":"ok"}`；`/accounts/D0000000001` 返回 cust_id 的活期账户；`/balance` 返回余额字符串。

- [ ] **Step 4: 提交**

```bash
cd .
git add templates/bank/cmd/seed templates/bank/go.sum
git commit -m "feat(bank): add seed entrypoint (create db + schema + fixtures)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

> **Phase 1 完成标志**：bank 模板独立可 build/test/up/seed，验收 #2、#5、#6、#7、#8 已可在模板内部验证。接下来 Phase 2 把它嵌入 jiade CLI 并暴露 `list/init/up/down/seed`。

## Phase 2 — jiade CLI 命令实现

### Task 14: internal/template — registry + manifest + 逐字 copy

> ⚠️ **实施时已改 tar 方案**：原 `//go:embed all:templates`（embed.FS 目录）因 Go 不能嵌入嵌套 module 而改为 `//go:embed templates.tar`（go:generate 打包）+ `archive/tar` 解压。详细步骤与代码以 `.superpowers/sdd/task-14-brief.md` 为准（已更新为 tar 版）。下方原 embed.FS 代码保留作历史记录，**不再有效**。

**Files:**
- Create: `internal/template/manifest.go`
- Create: `internal/template/registry.go`
- Create: `internal/template/render.go`
- Create: `internal/template/manifest_test.go`
- Create: `internal/template/render_test.go`

**Interfaces:**
- Produces: `template.Manifest`（yaml 结构）+ 子类型 `Database/Service/Seed`；`template.New() (*Registry, error)`（用 `//go:embed all:templates` 发现内嵌模板）；`Registry.Names() ([]string, error)`；`Registry.Manifest(name) (*Manifest, error)`；`Registry.FS(name) (fs.FS, error)`；`template.Copy(name, r, dir, force) error`（逐字拷贝，文件落在 `dir/` 下；非空拒绝返回 `ErrDirNotEmpty`，`force` 合并覆盖）；`template.ErrDirNotEmpty`。
- Task 16 的 `init`/`list` 命令依赖这些签名。`go:embed all:templates` 用 `all:` 前缀确保 `.env.example` 等点号开头文件也被嵌入（验收 #8 自包含需要它）。
- 拷贝语义对齐 `cp -r templates/<name>/. <dir>/`：合并写入（同名覆盖，不删无关文件）；v1 零字符串替换。

- [ ] **Step 1: 加 yaml 依赖**

Run:
```bash
cd .
go get gopkg.in/yaml.v3@v3.0.1
go mod tidy
```

- [ ] **Step 2: 写 manifest 结构**

Create `internal/template/manifest.go`:
```go
// Package template 发现内嵌模板、解析清单、逐字渲染（copy）。
package template

// Manifest template.yaml 清单（设计文档 §6）。随工程拷贝，被 list/up/seed 读。
type Manifest struct {
	Name        string     `yaml:"name"`
	Description string     `yaml:"description"`
	Version     string     `yaml:"version"`
	Databases   []Database `yaml:"databases"`
	Services    []Service  `yaml:"services"`
	Seed        Seed       `yaml:"seed"`
}

// Database 某业务库及其迁移 SQL。
type Database struct {
	Name    string `yaml:"name"`
	Migrate string `yaml:"migrate"`
}

// Service 某服务及其端口、所属库。
type Service struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
	DB   string `yaml:"db"`
}

// Seed fixture 生成器入口与规模。
type Seed struct {
	Entrypoint string   `yaml:"entrypoint"`
	Scales     []string `yaml:"scales"`
}
```

- [ ] **Step 3: 写 registry（go:embed 发现 + manifest 解析）**

Create `internal/template/registry.go`:
```go
package template

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"gopkg.in/yaml.v3"
)

//go:embed all:templates
var embedded embed.FS

const templatesDir = "templates"

// Registry 已发现的内嵌模板集合。
type Registry struct {
	fsys fs.FS // templates/ 子树
}

// New 用内嵌的 templates/ 构造 registry。
func New() (*Registry, error) {
	sub, err := fs.Sub(embedded, templatesDir)
	if err != nil {
		return nil, fmt.Errorf("template: 内嵌 templates/ 缺失: %w", err)
	}
	return &Registry{fsys: sub}, nil
}

// Names 返回可用模板名（排序）。
func (r *Registry) Names() ([]string, error) {
	entries, err := fs.ReadDir(r.fsys, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Manifest 读某模板的 template.yaml。
func (r *Registry) Manifest(name string) (*Manifest, error) {
	data, err := fs.ReadFile(r.fsys, name+"/template.yaml")
	if err != nil {
		return nil, fmt.Errorf("template: 读 %s/template.yaml: %w", name, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("template: 解析 %s manifest: %w", name, err)
	}
	return &m, nil
}

// FS 返回某模板的子文件系统（render 用）。
func (r *Registry) FS(name string) (fs.FS, error) {
	if _, err := fs.Stat(r.fsys, name); err != nil {
		return nil, fmt.Errorf("template: 未知模板 %q", name)
	}
	return fs.Sub(r.fsys, name)
}
```

- [ ] **Step 4: 写 render（逐字 copy）**

Create `internal/template/render.go`:
```go
package template

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrDirNotEmpty 目标目录非空且未 --force。
var ErrDirNotEmpty = fmt.Errorf("目标目录非空（用 --force 覆盖）")

// Copy 把模板 name 逐字拷贝到 dir（文件直接落在 dir/ 下，零字符串替换）。
func Copy(name string, r *Registry, dir string, force bool) error {
	if err := checkTarget(dir, force); err != nil {
		return err
	}
	src, err := r.FS(name)
	if err != nil {
		return err
	}
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dir, path), 0o755)
		}
		return copyFile(src, path, filepath.Join(dir, path))
	})
}

func checkTarget(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return ErrDirNotEmpty
	}
	return os.MkdirAll(dir, 0o755)
}

func copyFile(fsys fs.FS, src, dst string) error {
	in, err := fsys.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
```

- [ ] **Step 5: 写 manifest + render 单测**

Create `internal/template/manifest_test.go`:
```go
package template

import "testing"

func TestNew_DiscoversBank(t *testing.T) {
	r := mustRegistry(t)
	names, err := r.Names()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "bank" {
			found = true
		}
	}
	if !found {
		t.Errorf("bank 未在模板列表: %v", names)
	}
}

func TestManifest_Bank(t *testing.T) {
	r := mustRegistry(t)
	m, err := r.Manifest("bank")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "bank" {
		t.Errorf("name=%q want bank", m.Name)
	}
	if len(m.Services) != 1 || m.Services[0].Name != "core-banking" || m.Services[0].Port != 8080 {
		t.Errorf("services=%+v", m.Services)
	}
	if len(m.Databases) != 1 || m.Databases[0].Name != "core_db" {
		t.Errorf("databases=%+v", m.Databases)
	}
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
```

Create `internal/template/render_test.go`:
```go
package template

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopy_CreatesFullProject(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := Copy("bank", r, dir, false); err != nil {
		t.Fatal(err)
	}
	must := []string{
		"go.mod", "go.sum", "docker-compose.yaml", "Dockerfile",
		"template.yaml", ".env.example", "Makefile",
		"README.md", "ARCHITECTURE.md",
		"cmd/core-banking/main.go", "cmd/seed/main.go",
		"db/migrations/core_db.sql",
		"internal/corebanking/domain/money.go",
		"internal/corebanking/service/ledger_service.go",
		"internal/fixtures/domains/core.go",
	}
	for _, p := range must {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("拷贝后缺失 %s: %v", p, err)
		}
	}
}

func TestCopy_RejectsNonEmpty(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Copy("bank", r, dir, false); err != ErrDirNotEmpty {
		t.Errorf("非空应返回 ErrDirNotEmpty, got %v", err)
	}
}

func TestCopy_ForceAllowsNonEmpty(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Copy("bank", r, dir, true); err != nil {
		t.Errorf("force 应允许非空目录: %v", err)
	}
}

func TestCopy_IsVerbatim(t *testing.T) {
	r := mustRegistry(t)
	dir := t.TempDir()
	if err := Copy("bank", r, dir, false); err != nil {
		t.Fatal(err)
	}
	src, err := r.FS("bank")
	if err != nil {
		t.Fatal(err)
	}
	orig, err := src.Open("db/migrations/core_db.sql")
	if err != nil {
		t.Fatal(err)
	}
	defer orig.Close()
	want, _ := io.ReadAll(orig)
	got, err := os.ReadFile(filepath.Join(dir, "db/migrations/core_db.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("拷贝应逐字一致")
	}
}
```
> `TestCopy_IsVerbatim` 需 `import "io"`，render_test.go 顶部补 `"io"`。

- [ ] **Step 6: 运行测试确认通过**

Run:
```bash
cd .
go test ./internal/template/...
```
Expected: PASS（bank 被发现、manifest 解析正确、copy 产出完整工程且逐字一致、非空拒绝/force 放行）。

- [ ] **Step 7: 提交**

```bash
cd .
git add internal/template go.mod go.sum
git commit -m "feat: add template registry, manifest, verbatim copy

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 15: internal/docker — docker/compose/daemon 探测

**Files:**
- Create: `internal/docker/probe.go`
- Create: `internal/docker/probe_test.go`

**Interfaces:**
- Produces: `docker.Commander` 接口（`Output(ctx, name, args...) ([]byte, error)`，单测注入假实现）；`docker.Probe(ctx) ProbeResult`（用真 exec）；`docker.ProbeWith(ctx, cmd) ProbeResult`；`ProbeResult{HasDocker, HasCompose, DaemonRunning}`；`ProbeResult.OK() bool`（up 前置：三者皆真）；`ProbeResult.Hint() string`（失败时的安装/启动提示）。
- Task 17 的 `up` 命令在 compose 前调用 `Probe`，`!OK()` 则失败并打印 `Hint`（设计文档 §5.1/§10）。

- [ ] **Step 1: 写探测单测（假 commander）**

Create `internal/docker/probe_test.go`:
```go
package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeCmd struct {
	ok map[string]bool // key = "name arg1 arg2 ..."
}

func (f fakeCmd) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	if f.ok[key] {
		return []byte("ok"), nil
	}
	return nil, errors.New("exec: not found")
}

func TestProbe_AllPresent(t *testing.T) {
	cmd := fakeCmd{ok: map[string]bool{
		"docker --version":     true,
		"docker compose version": true,
		"docker info":          true,
	}}
	r := ProbeWith(context.Background(), cmd)
	if !r.OK() {
		t.Errorf("应 OK, got %+v hint=%q", r, r.Hint())
	}
}

func TestProbe_NoDocker(t *testing.T) {
	r := ProbeWith(context.Background(), fakeCmd{ok: map[string]bool{}})
	if r.HasDocker {
		t.Error("不应有 docker")
	}
	if r.OK() {
		t.Error("无 docker 不应 OK")
	}
	if !strings.Contains(r.Hint(), "安装") {
		t.Errorf("无 docker 提示应含'安装', got %q", r.Hint())
	}
}

func TestProbe_DaemonDown(t *testing.T) {
	cmd := fakeCmd{ok: map[string]bool{
		"docker --version":       true,
		"docker compose version": true,
	}}
	r := ProbeWith(context.Background(), cmd)
	if r.DaemonRunning {
		t.Error("daemon 不应运行")
	}
	if !strings.Contains(r.Hint(), "Docker Desktop") {
		t.Errorf("daemon 未运行提示应含'Docker Desktop', got %q", r.Hint())
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run:
```bash
cd .
go test ./internal/docker/...
```
Expected: FAIL（`ProbeWith`/`ProbeResult` 未定义）。

- [ ] **Step 3: 写探测实现**

Create `internal/docker/probe.go`:
```go
// Package docker 探测 docker/compose/daemon 环境（up 命令前置）。
package docker

import (
	"context"
	"os/exec"
)

// Commander 执行命令的抽象（单测注入假实现）。
type Commander interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

type realCommander struct{}

func (realCommander) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// ProbeResult docker 环境探测结果。
type ProbeResult struct {
	HasDocker     bool
	HasCompose    bool // docker compose 子命令可用
	DaemonRunning bool
}

// Probe 用真 exec 探测。
func Probe(ctx context.Context) ProbeResult {
	return ProbeWith(ctx, realCommander{})
}

// ProbeWith 用注入的 commander 探测（可单测）。
func ProbeWith(ctx context.Context, cmd Commander) ProbeResult {
	res := ProbeResult{}
	if _, err := cmd.Output(ctx, "docker", "--version"); err != nil {
		return res
	}
	res.HasDocker = true
	if _, err := cmd.Output(ctx, "docker", "compose", "version"); err == nil {
		res.HasCompose = true
	}
	if _, err := cmd.Output(ctx, "docker", "info"); err == nil {
		res.DaemonRunning = true
	}
	return res
}

// OK up 前置：docker + compose + daemon 皆就绪。
func (p ProbeResult) OK() bool {
	return p.HasDocker && p.HasCompose && p.DaemonRunning
}

// Hint 失败时的人类可读提示。
func (p ProbeResult) Hint() string {
	switch {
	case !p.HasDocker:
		return "未检测到 docker，请先安装 Docker"
	case !p.HasCompose:
		return "未检测到 docker compose 子命令，请升级 Docker"
	case !p.DaemonRunning:
		return "docker daemon 未运行，请先启动 Docker Desktop"
	default:
		return ""
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run:
```bash
cd .
go test ./internal/docker/...
```
Expected: PASS（三种探测场景）。

- [ ] **Step 5: 提交**

```bash
cd .
git add internal/docker
git commit -m "feat: add docker/compose/daemon probe

Co-Authored-By: Claude <noreply@anthropic.com>"
```

### Task 16: internal/ui + list/init 命令（替换 Task 1 桩）

**Files:**
- Create: `internal/ui/ui.go`
- Modify: `internal/cli/list.go`（替换 Task 1 桩）
- Modify: `internal/cli/init.go`（替换 Task 1 桩）
- Create: `internal/cli/list_test.go`

**Interfaces:**
- Consumes: `template.Registry/Names/Manifest/Copy/ErrDirNotEmpty`（Task 14）；`promptui`（交互选择）。
- Produces: `ui.New(out,err) *UI`；`UI.Step/OK/Warn/Error`（符号前缀输出，无颜色库依赖，跨平台）；`list` 命令（打印模板名+描述）；`init` 命令（交互或 `--template`+`--dir` 全非交互 → `template.Copy`）。
- init 行为（设计文档 §5.1）：未给 `--template` → promptui 选模板；未给 `--dir` → promptui 输入目录；`template.Copy` 逐字拷贝；非空拒绝（`--force` 合并覆盖）。

- [ ] **Step 1: 写 ui 包**

Create `internal/ui/ui.go`:
```go
// Package ui 提供 jiade 的终端输出（符号前缀，无颜色库依赖）。
package ui

import (
	"fmt"
	"io"
)

type UI struct {
	Out io.Writer
	Err io.Writer
}

func New(out, errw io.Writer) *UI {
	return &UI{Out: out, Err: errw}
}

func (u *UI) Step(format string, args ...any)  { fmt.Fprintf(u.Out, "▶ "+format+"\n", args...) }
func (u *UI) OK(format string, args ...any)    { fmt.Fprintf(u.Out, "✓ "+format+"\n", args...) }
func (u *UI) Warn(format string, args ...any)  { fmt.Fprintf(u.Err, "! "+format+"\n", args...) }
func (u *UI) Error(format string, args ...any) { fmt.Fprintf(u.Err, "✗ "+format+"\n", args...) }
```

- [ ] **Step 2: 覆盖 list 命令（替换桩）**

Overwrite `internal/cli/list.go` with:
```go
package cli

import (
	"github.com/projanvil/jiade/internal/template"
	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出可用模板",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := template.New()
			if err != nil {
				return err
			}
			names, err := reg.Names()
			if err != nil {
				return err
			}
			u := ui.New(opts.Stdout, opts.Stderr)
			if len(names) == 0 {
				u.Warn("无可用模板")
				return nil
			}
			for _, n := range names {
				desc := ""
				if m, err := reg.Manifest(n); err == nil {
					desc = m.Description
				}
				u.Step("%s — %s", n, desc)
			}
			return nil
		},
	}
}
```

- [ ] **Step 3: 覆盖 init 命令（替换桩）**

Overwrite `internal/cli/init.go` with:
```go
package cli

import (
	"fmt"
	"strings"

	"github.com/manifoldco/promptui"
	"github.com/projanvil/jiade/internal/template"
	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newInitCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "从模板拷贝出一个工程（逐字拷贝，零替换）",
		RunE: func(cmd *cobra.Command, args []string) error {
			tplName, _ := cmd.Flags().GetString("template")
			force, _ := cmd.Flags().GetBool("force")
			dir := opts.Dir

			reg, err := template.New()
			if err != nil {
				return err
			}
			u := ui.New(opts.Stdout, opts.Stderr)

			if tplName == "" {
				if tplName, err = promptTemplate(reg); err != nil {
					return err
				}
			}
			if dir == "" {
				if dir, err = promptDir(); err != nil {
					return err
				}
			}
			u.Step("拷贝模板 %s → %s", tplName, dir)
			if err := template.Copy(tplName, reg, dir, force); err != nil {
				return err
			}
			u.OK("完成。下一步：cd %s && jiade up && jiade seed", dir)
			return nil
		},
	}
	cmd.Flags().String("template", "", "模板名（如 bank）")
	cmd.Flags().Bool("force", false, "目标目录非空时强制覆盖")
	return cmd
}

func promptTemplate(reg *template.Registry) (string, error) {
	names, err := reg.Names()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "", fmt.Errorf("无可用模板")
	}
	sel := promptui.Select{Label: "选择模板", Items: names}
	_, result, err := sel.Run()
	return result, err
}

func promptDir() (string, error) {
	p := promptui.Prompt{
		Label: "目标目录（工程根，文件直接落在此目录下）",
		Validate: func(s string) error {
			if strings.TrimSpace(s) == "" {
				return fmt.Errorf("不能为空")
			}
			return nil
		},
	}
	return p.Run()
}
```

- [ ] **Step 4: 写 list 单测**

Create `internal/cli/list_test.go`:
```go
package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestList_PrintsBank(t *testing.T) {
	opts := &Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	cmd := newListCmd(opts)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatal(err)
	}
	out := opts.Stdout.(*bytes.Buffer).String()
	if !strings.Contains(out, "bank") {
		t.Errorf("list 输出应含 bank: %q", out)
	}
}
```

- [ ] **Step 5: 运行测试确认通过**

Run:
```bash
cd .
go build ./...
go test ./internal/cli/... ./internal/ui/...
```
Expected: 编译通过；list 测试输出含 bank。

- [ ] **Step 6: 提交**

```bash
cd .
git add internal/ui internal/cli
git commit -m "feat: implement list + init commands with promptui

Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 17: up/down/seed 命令（替换 Task 1 桩）

**Files:**
- Modify: `internal/cli/compose.go`（替换 Task 1 桩：up/down + runCompose helper）
- Modify: `internal/cli/seed.go`（替换 Task 1 桩）

**Interfaces:**
- Consumes: `docker.Probe/ProbeResult`（Task 15）；`ui`（Task 16）。
- Produces: `up`（探测 docker → `docker compose up -d [--build]`，cwd=dir，透传 stderr+退出码）；`down`（`docker compose down`）；`seed`（cwd=dir 跑 `go run ./cmd/seed --scale=… [--reset]`，透传 stderr+退出码，失败提示"先 jiade up"）；私有 `runCompose(stderr, dir, args...)` helper。
- 设计文档 §10：up 前置探测（缺失/未运行则失败+提示）；子进程 stderr 原文透传、退出码透传；seed 失败包一层"先 up"。

- [ ] **Step 1: 覆盖 compose.go（up/down + helper）**

Overwrite `internal/cli/compose.go` with:
```go
package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/projanvil/jiade/internal/docker"
	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "在目标目录内 docker compose up -d（前置探测 docker）",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				return fmt.Errorf("需要 --dir 指定目标工程目录")
			}
			build, _ := cmd.Flags().GetBool("build")
			u := ui.New(opts.Stdout, opts.Stderr)

			probe := docker.Probe(cmd.Context())
			if !probe.OK() {
				return fmt.Errorf("%s", probe.Hint())
			}
			u.Step("docker compose up（%s）", dir)
			upArgs := []string{"up", "-d"}
			if build {
				upArgs = append(upArgs, "--build")
			}
			return runCompose(opts.Stderr, dir, upArgs...)
		},
	}
	cmd.Flags().Bool("build", false, "compose up 时强制 --build")
	return cmd
}

func newDownCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "在目标目录内 docker compose down",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				return fmt.Errorf("需要 --dir 指定目标工程目录")
			}
			ui.New(opts.Stdout, opts.Stderr).Step("docker compose down（%s）", dir)
			return runCompose(opts.Stderr, dir, "down")
		},
	}
}

// runCompose 在 dir 内执行 docker compose，stdout/stderr 透传，退出码透传。
func runCompose(stderr io.Writer, dir string, args ...string) error {
	c := exec.Command("docker", append([]string{"compose"}, args...)...)
	c.Dir = dir
	c.Stdout = os.Stdout
	c.Stderr = stderr
	return c.Run()
}
```

- [ ] **Step 2: 覆盖 seed.go**

Overwrite `internal/cli/seed.go` with:
```go
package cli

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/projanvil/jiade/internal/ui"
	"github.com/spf13/cobra"
)

func newSeedCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "运行目标工程的 fixture 生成器",
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.Dir
			if dir == "" {
				return fmt.Errorf("需要 --dir 指定目标工程目录")
			}
			scale, _ := cmd.Flags().GetString("scale")
			reset, _ := cmd.Flags().GetBool("reset")
			ui.New(opts.Stdout, opts.Stderr).Step("seed（%s, scale=%s reset=%v）", dir, scale, reset)

			c := exec.Command("go", "run", "./cmd/seed", "--scale="+scale)
			if reset {
				c.Args = append(c.Args, "--reset")
			}
			c.Dir = dir
			c.Stdout = os.Stdout
			c.Stderr = opts.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("%w（请先 jiade up 启动 postgres）", err)
			}
			return nil
		},
	}
	cmd.Flags().String("scale", "dev", "规模：dev|full")
	cmd.Flags().Bool("reset", false, "重建库与表（幂等）")
	return cmd
}
```

- [ ] **Step 3: 编译 + 全量单测**

Run:
```bash
cd .
go build ./...
go test ./...
go vet ./...
```
Expected: jiade module 编译通过；全部单测 PASS（cli/docker/template/ui；不含 templates/bank——它是独立 module）。`go test ./...` 不递归进 templates/bank（其有自身 go.mod）。

- [ ] **Step 4: 提交**

```bash
cd .
git add internal/cli
git commit -m "feat: implement up/down/seed commands with docker probe + passthrough

Co-Authored-By: Claude <noreply@anthropic.com>"
```

## Phase 3 — 端到端冒烟 + CI

### Task 18: e2e 冒烟 + CI + 验收对照

**Files:**
- Create: `Makefile`（jiade 仓根）
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Produces: jiade 仓根 `Makefile`（`test`/`bank-test`/`e2e`）；CI 三 job（jiade build/test、bank 独立 module build/test、e2e 端到端）。覆盖设计文档 §11 全部 8 条验收。

- [ ] **Step 1: 写 jiade 仓根 Makefile**

Create `Makefile`:
```makefile
.PHONY: generate test bank-test e2e clean

# 打包 templates/bank → templates.tar（go:embed 需要；改模板后重跑）
generate:
	go generate ./internal/template

# jiade 自身（不含 templates/bank——它是独立 module）
test: generate
	go build ./...
	go test ./...

# bank 模板作为独立 module 验证（验收 #2）
bank-test:
	cd templates/bank && go build ./... && go test ./...

# 端到端冒烟（需 docker；验收 #5）
e2e: generate
	rm -rf /tmp/jiade-e2e
	go run ./cmd/jiade init --template bank --dir /tmp/jiade-e2e --force
	cd /tmp/jiade-e2e && docker compose up -d --build
	cd /tmp/jiade-e2e && go run ./cmd/seed --scale=dev --reset
	sleep 5
	curl -sf localhost:8080/healthz
	curl -sf localhost:8080/api/v1/accounts/D0000000001
	curl -sf "localhost:8080/api/v1/accounts/D0000000001/balance"
	@echo "E2E OK"

clean:
	rm -rf /tmp/jiade-e2e
```

- [ ] **Step 2: 写 CI workflow**

Create `.github/workflows/ci.yml`:
```yaml
name: ci
on: [push, pull_request]

jobs:
  jiade:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: go generate ./internal/template
      - run: go build ./...
      - run: go test ./...

  bank:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: cd templates/bank && go build ./... && go test ./...

  e2e:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - run: make e2e
```

- [ ] **Step 3: 本地跑 e2e 冒烟（需 docker）**

Run:
```bash
cd .
make e2e
```
Expected:
- `jiade init` 在 `/tmp/jiade-e2e` 产出完整 bank 工程；
- `docker compose up` 起 postgres + core-banking；
- `go run ./cmd/seed --reset` 打印"完成 ✅ 活期 2000…"；
- `curl /healthz` → `{"status":"ok"}`；
- `curl /accounts/D0000000001` → 含 `"cust_id"` 的活期账户；
- `curl /balance` → 含 `"balance"` 字符串；
- 末行打印 `E2E OK`。

- [ ] **Step 4: 提交**

```bash
cd .
git add Makefile .github
git commit -m "ci: add makefile + github actions (jiade/bank/e2e)

Co-Authored-By: Claude <noreply@anthropic.com>"
```

- [ ] **Step 5: 验收标准对照（设计文档 §11）**

| # | 验收 | 实现位置 |
|---|------|---------|
| 1 | jiade 仓 `go build ./...` + `go test ./...` 全绿（不含 templates/） | Task 1/14/15/16/17 + CI `jiade` job |
| 2 | `templates/bank/` 拷到临时目录后 build+test 全绿（独立 module） | Task 13 Step 2 + CI `bank` job（`cd templates/bank`） |
| 3 | `jiade list` 列出 bank | Task 16（`TestList_PrintsBank`） |
| 4 | `init` 空目录产出完整工程；非空拒绝；`--force` 覆盖 | Task 14（render 测试）+ Task 16 init |
| 5 | `init → up → seed` 后 `curl /healthz` 200 + 真实账户 + 余额 | Task 18 `make e2e` |
| 6 | 同 Seed+Scale 两次 seed 一致（确定性） | Task 12（domains 确定性测试） |
| 7 | 复式记账不平被单测拦截 | Task 6（`TestPost_Unbalanced_RefusesAndDoesNotTouchStore`） |
| 8 | 生成物在无 jiade 环境也能 compose up + go run（自包含） | Task 2（bank Makefile/README）+ Task 13 模板本地冒烟 |

---

## Self-Review

### 1. Spec 覆盖（逐节核对设计文档）

- §1–§4（定位/目标/原则/架构）：双层布局、自闭、自包含、init 纯 copy、缩影哲学 → Global Constraints 锁定；File Structure 映射双层。
- §5（CLI 命令面/选型）：list/init/up/down/seed、`--build`/`--scale`/`--reset`/`--force`/`--dir`、init 不需 docker 仅警告、up 前置探测 → Task 1/16/17。
- §5.2（模板 Go 代码不参与 jiade 编译，靠拷出再 build/test）→ Task 2（bank 独立 go.mod）+ Task 14（embed）+ Task 18（CI bank job）。
- §6（template.yaml 契约）→ Task 2 写入 + Task 14 解析。
- §7（core-banking 分层、复式记账不变量、DB 拓扑、只读 API）→ Task 4–10。
- §7.2 不变量（金额 int64 分、复式平衡、依赖向内）→ Task 4（money 禁 float + 守卫）、Task 6（ValidateBalance + 不平拒绝）、分层接口（service 依赖倒置，repo→domain）。
- §8（fixture 移植 + Spec A 范围划线）→ Task 11/12；core-banking 自包含、基线 balance + 少量 txn 简化已注明。
- §9（三层测试）→ Task 6/9/12/15 单测；Task 10 集成测试（build tag）；Task 18 端到端。
- §10（错误处理：docker 缺失/daemon/非空/端口/seed 失败/stderr 透传/--reset/--force）→ Task 14（非空拒绝/force）、Task 15（探测+Hint）、Task 17（stderr+退出码透传、seed 失败提示）、Task 13（seed 重试）。
- §11（8 条验收）→ Task 18 Step 5 对照表，全部命中。
- §12/§13（Spec B/开放问题）→ 非本 spec 范围，文件结构与契约已按 7 服务容量设计（单 module + cmd 多入口 + 单库可扩多库）。

### 2. Placeholder 扫描

无 TBD/TODO/"implement later"/"add error handling" 等占位。所有代码步骤含完整可编译代码；所有测试步骤含完整测试代码与断言；所有命令步骤含确切命令与预期输出。

### 3. 类型一致性

- `domain.BalanceDelta{AccountNo string; Delta Money; SubjectCode string}`：Task 5 定义（domain 包）→ Task 6 `LedgerStore.ApplyBalanceDeltas(ctx, bizDate string, deltas []domain.BalanceDelta)` → Task 8 `LedgerRepo.ApplyBalanceDeltas` 同签名 → Task 10 集成测试用 `domain.BalanceDelta`。一致。
- `domain.Money`：Task 4 定义 `NewMoneyFromCents/ParseCents/String/Add/Sub/Cents`；Task 8 repo 用 `ParseCents`/`String`；Task 9 api 用 `String()`；Task 12 fixture 用 `NewMoneyFromCents`。一致。
- `service.LedgerStore`（InsertTxns/ApplyBalanceDeltas/UpsertGL）、`service.AccountStore`（InsertDemand/InsertFixed/SetDemandStatus）、`service.TxnStore`（ListTxns/GetLatestBalance）：Task 6/7 定义 → Task 8 repo 逐字实现。一致。
- `api.AccountReader`（GetDemand/GetFixed）、`api.LedgerReader`（GetGL）：Task 9 定义 → Task 8 `AccountRepo`/`LedgerRepo` 实现这些方法。一致。
- `template.Copy(name, r, dir, force)` 与 `template.ErrDirNotEmpty`：Task 14 定义 → Task 16 init 调用 + 断言。一致。
- `docker.Probe(ctx) ProbeResult` + `OK()/Hint()`：Task 15 定义 → Task 17 up 调用。一致。

---

## Execution Handoff

计划已保存至 `docs/superpowers/plans/2026-07-15-jiade-spec-a.md`。两种执行方式：

**1. Subagent-Driven（推荐）** — 每个任务派一个全新 subagent，任务间 review，快速迭代。
**2. Inline Execution** — 在本会话用 executing-plans 逐任务执行，带 checkpoint 批量执行。

**选哪种？**
