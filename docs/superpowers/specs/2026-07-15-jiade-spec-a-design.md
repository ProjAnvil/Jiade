# Jiade — Spec A 设计文档

- **日期**：2026-07-15
- **状态**：设计待审
- **范围**：Spec A —— Jiade CLI 框架 + `bank` 模板纵切（core-banking 单服务）
- **作者**：yuhaochen × Claude（brainstorming）

---

## 1. 背景与定位

### 1.1 Jiade 是什么

Jiade 是一套 **「现实世界大工程的缩影」生成器**：一个 Go CLI（`jiade`），用户执行 `jiade init --template bank`，即可在指定目录拷贝出一个**简化但真实、可运行**的行业系统工程（代码 + 数据 + 部署拓扑）。

每个模板不是玩具，而是一个真实行业系统**按比例浓缩**的版本——保留的是「模式与约定」（架构分层、金融不变量、领域建模），砍掉的是「规模与边角」。这正是其价值所在。

### 1.2 生态上下文（Jiade 与下游零联动）

Jiade 属于 `projanvil` 工具链。它的产物（干净、地道的 Go 工程）**可被**下游工具消费——例如 SCV（代码→知识库）、Porto（PRD→技术规格）——但 **Jiade 本身对这些工具零感知、零联动**。Jiade 只关心自己：生成可运行的行业缩影工程。是否把产物喂给下游、何时喂，由用户自行决定，Jiade 不探测、不调用、不引导。

### 1.3 关键推论

Jiade 生成的代码**必须真实到能代表一个真实工程**——分层、金融不变量、领域建模、自文档化。因为它是「现实世界大工程的缩影」，不是玩具或样板。

---

## 2. 目标与非目标

### 2.1 Spec A 目标

1. **Jiade CLI 框架**（Go/Cobra）：`list` / `init` / `up` / `down` / `seed`，`up` 内置 Docker 探测前置。
2. **模板契约 + 渲染机制**：`template.yaml` 清单；`init` = 纯 copy-paste（逐字拷贝，v1 零字符串替换）。
3. **`bank` 模板纵切**：一份完整、地道的 Go 参考工程，含 `core-banking` 单服务（domain/repo/service/api 分层 + 复式记账不变量）+ Go fixture 生成器 + docker-compose（1 Go 服务 + Postgres）。
4. **三层测试**：CLI 单测、模板自带测试、端到端冒烟。

### 2.2 Spec A 非目标（留给 Spec B 或以后）

- bank 的其余 6 个服务（customer/payment/reward/risk/loan/wealth）及其 schema/fixture/API。
- 多库 + FDW 跨库联邦。
- 完整多日切日引擎（逐日推进 + 三因子分布）。
- 其他行业模板（零售/物流/医疗…）。
- `--name` 重命名、端口自动分配。
- **与 SCV/Porto 的任何联动**（探测/调用/引导/包装命令）——Jiade 永远自闭，不感知下游。

> **Spec A 的模板契约、每服务目录布局、compose 结构从一开始就按 7 服务容量设计**，所以 Spec B 是「填内容」，不是「改架构」。bank 的完整 7 服务拓扑（C2 终态）是 Spec B 的交付物，本 spec 只交付纵切骨架。

---

## 3. 核心原则

| 原则 | 含义 |
|------|------|
| **生成物自包含** | 拷出的工程离开 `jiade` 也能 `docker compose up` 和 `go run`；`jiade` 只是切进目标目录的薄编排器，非运行时依赖。 |
| **代码是头号产出** | 生成的是一整个真实分层 Go 工程，fixture 只是其中 `cmd/seed` 一个组件。 |
| **init 纯 copy-paste** | `cp -r templates/<template>/. <dir>/`，逐字拷贝，v1 零替换。 |
| **地道可读 by construction** | 生成物自带 README/ARCHITECTURE、地道分层、go.mod、测试——它是真实工程缩影，天生清晰可读。 |
| **自闭** | Jiade 只关心自己，不与 SCV/Porto 等下游工具联动。 |
| **缩影哲学** | 简化但真实：保留模式与不变量，砍掉规模与边角。 |

---

## 4. 架构总览（双层布局）

### 4.1 第 1 层 — Jiade 工具仓（Spec A 要建的）

```
jiade/                              # module: github.com/<you>/jiade
├── cmd/jiade/main.go               # CLI 入口
├── internal/
│   ├── cli/                        # Cobra: list/init/up/down/seed
│   ├── docker/                     # 探测 docker + compose + daemon（up 前置）
│   ├── template/                   # registry（发现 templates/）+ manifest + render(=copy)
│   └── ui/                         # 输出着色/进度
├── templates/                      # 内嵌模板（go:embed）
│   └── bank/                       # ← Spec A 唯一模板（即第 2 层的源）
└── README.md
```

### 4.2 第 2 层 — `jiade init` 生成的产物（独立、可读的 Go 工程）

```
<dir>/                              # module: bank （模板内 go.mod 写死，逐字拷贝）
├── go.mod
├── README.md  ARCHITECTURE.md      # 工程自带说明
├── docker-compose.yaml             # postgres + core-banking
├── .env.example  Makefile          # 不装 jiade 也能 up/down/seed/test
├── cmd/
│   ├── core-banking/main.go        # API 服务入口
│   └── seed/main.go                # fixture 生成器入口
├── internal/
│   ├── platform/                   # pg 连接 + migration runner
│   ├── corebanking/                # 服务：domain/repo/service/api
│   └── fixtures/                   # 生成器：config/rng/domains/core
├── db/migrations/core_db.sql       # 核心账务库 DDL（10 表）
└── template.yaml                   # 随工程拷贝，被 list/up/seed 读取
```

> 单 `go.mod`、`cmd/` 多入口的 mono-repo 风格：是一个连贯工程；Spec B 加服务只是 `cmd/` 加入口、`internal/` 加域包，零架构改动。

### 4.3 数据流

```
jiade init --template bank --dir ./mybank
   └─ copy templates/bank/. → ./mybank/
jiade up        └─ cd ./mybank && docker compose up -d --build
jiade seed      └─ cd ./mybank && go run ./cmd/seed --scale=dev --reset
                 （seed 自带建库→建表→灌数据，幂等）
（到此为止。下游如何消费产物，由用户自行决定。）
```

---

## 5. Jiade CLI

### 5.1 命令面

```
jiade                                    # 帮助
jiade list                               # 列可用模板（Spec A: bank）
jiade init                               # 交互式：↑↓选模板 → 选目录（当前目录 or 输入路径）
jiade init --template bank               # 给定模板，提示选目录
jiade init --template bank --dir /xxx    # 全非交互：拷贝到 /xxx/
jiade init --dir /xxx                    # 给定目录，提示选模板
jiade up   [--build]                     # 目标目录内 docker compose up -d（前置探测 docker）
jiade down                               # docker compose down
jiade seed [--scale dev|full] [--reset]  # 跑目标的 fixture 生成器
```

**`init` 行为**：
- 机制 = `cp -r templates/<template>/. <dir>/`，逐字拷贝，**v1 零字符串替换**。
- `--dir` 指工程根目录本身（文件直接落在 `<dir>/` 下）；不存在则创建；已存在且非空 → 拒绝，`--force` 才覆盖。
- 交互选择器用轻量库（`promptui`/`survey`），↑↓选。
- `init` 不需要 docker → docker 缺失仅警告，照常脚手架。

**`up` 行为**：执行前探测 `docker` / `docker compose` / daemon；缺失或未运行则失败并给清晰提示，不进入 compose。

### 5.2 技术选型

| 关注点 | 选型 | 理由 |
|--------|------|------|
| CLI 框架 | Cobra | 与既有 go 技能栈一致 |
| 服务 API | net/http + chi | 零重量，「缩影」不背 Fiber/GORM |
| DB 访问 | database/sql + pgx | raw SQL 风格，读起来直白 |
| 模板内嵌 | go:embed | 模板随二进制分发，无外部依赖 |
| 交互选择 | promptui/survey | 轻、够用 |
| 渲染 | （v1 无） | 纯 copy；以后需要替换再加 text/template |

> **模板 Go 代码的构建/测试**：`templates/bank/` 是一个**独立 Go module**（自带 `go.mod: module bank`），通过 `go:embed` 作为**静态资源**内嵌，**不**参与 jiade 自身的 `go build ./...` 编译。它的正确性靠「拷出来再 build/test」保证——即端到端冒烟（§9.3）与验收 #5/#7。jiade 仓的 CI 需额外加一步：把 `templates/bank/` 拷到临时目录跑 `go build ./... && go test ./...`。

---

## 6. 模板契约 `template.yaml`

随工程拷贝，被 `list`/`up`/`seed` 读取；`init` 不读它（init 只认 `templates/<name>/` 目录）。

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

---

## 7. bank 模板（生成物内部）

### 7.1 core-banking 服务分层（真实 clean architecture，非 stub）

```
internal/
├── platform/                     # 基础设施（可替换、不碰业务）
│   ├── pg/          # 连接池 + 事务封装
│   └── migrate/     # 跑 core_db.sql
├── corebanking/
│   ├── domain/                   # 纯领域模型（零 DB/框架依赖，最内层）
│   │   ├── account.go   # 活期/定存账户、账户状态机
│   │   ├── money.go     # Money 用 int64 分表示（金融禁 float）
│   │   ├── ledger.go    # 科目、总账、借贷记账
│   │   └── txn.go       # 流水、借贷标志、渠道
│   ├── repo/                     # 仓储层（pgx 落库）
│   │   ├── account_repo.go  txn_repo.go  ledger_repo.go
│   ├── service/                  # 用例层（业务规则，纯逻辑、可单测）
│   │   ├── account_service.go   # 开户/销户/状态流转
│   │   ├── ledger_service.go    # 过账：借必等于贷，更新分户账+总账
│   │   └── txn_service.go       # 记账、冲正
│   └── api/                      # 传输层（http handlers + router）
│       ├── account_handler.go  ledger_handler.go  health.go
cmd/
├── core-banking/main.go          # 依赖装配、起 server
└── seed/main.go                  # fixture 生成器入口
```

依赖方向向内：`api → service → repo → domain`，`domain` 不依赖任何人。

### 7.2 写死的真实银行不变量（缩影该体现的真实工程约定）

1. **金额用 int64 分**，禁止 float（`money.go` + 全链路遵守）。
2. **复式记账平衡**：`ledger_service.Post()` 强制 `sum(借)==sum(贷)`，不平回滚事务 + 单测覆盖。
3. **依赖方向向内**，单向无环。

### 7.3 DB 拓扑

- Spec A：**一个 Postgres 容器 + 一个库 `core_db`**。
- `seed` 二进制负责 建库 → 建表（`core_db.sql`）→ 灌数据，幂等（`--reset` 先 DROP 再建）。
- `core-banking` 服务对库**只读**（查询接口），假设 schema+数据已由 seed 就绪。
- **Spec B 扩展点**（架构不动）：每加一个服务 = 加一个库（`cust_db`/`pay_db`/…）+ FDW 联邦视图。

### 7.4 只读 API（Spec A）

- `GET /healthz` —— 存活检查。
- `GET /api/v1/accounts/{account_no}` —— 查账户。
- `GET /api/v1/accounts/{account_no}/balance` —— 查余额（取最新 biz_date 的 account_balance）。
- `GET /api/v1/txns?account_no=...&from=...&to=...` —— 查流水。
- `GET /api/v1/ledger?biz_date=...` —— 查总账。

写操作（过账/记账）在 Spec A 的 `service` 层实现并有单测，但**暂不暴露 HTTP 写接口**（避免与 seed 的数据权威性冲突）；写接口留 Spec B。

---

## 8. fixture 生成器（Python → Go 移植）

| 原型 (Python) | Jiade (Go) `internal/fixtures/` |
|---|---|
| `Scale` enum + `TARGET_COUNTS` | `config.go`：`Scale{Dev,Full}` + 目标量级表 |
| `FixtureConfig`(日期范围, seed=42) | `config.go`：`Config{StartDate,EndDate,Scale,Seed}` |
| `make_faker(zh_CN, seed)` | `rng.go`：`math/rand` 定步长种子 + 手写小词库（姓名/商户/渠道），确定性、零重依赖 |
| `gen_static`（币种/机构/科目/利率/sys_param） | `domains/core.go: GenStatic` |
| `gen_accounts`（活期/定期 + 客户-账户关系） | `domains/core.go: GenAccounts` |
| `bizdate.py`（400 业务日切日 + 每日流水曲线 + 总账滚存） | **Spec B 才完整移植** |

**Spec A fixture 范围划线**：静态主数据 + 活/定存账户 + 基线 `account_balance` + 少量近期 `acct_txn`。完整多日切日引擎（逐日推进 + 三因子分布）属 Spec B。

**确定性**：同 `Seed`+`Scale` → 同样的行（用于可复现 + 单测哈希比对）。

---

## 9. 测试策略

1. **Jiade CLI 单测**（`internal/template`、`internal/docker`）：清单解析、模板发现、`init` 拷贝到临时目录后断言文件齐全且逐字一致；docker 探测用接口抽象，单测不依赖真 docker。
2. **bank 模板自带测试**（随工程拷走）：
   - fixture 确定性（同 Seed+Scale → 同样的行，哈希比对）+ `--reset` 幂等。
   - `service` 层纯逻辑单测：复式记账不平必失败、金额禁 float、账户状态机。
   - `repo`+`api` 对真 Postgres（CI 里 docker）的集成测试。
3. **端到端冒烟**：Makefile/CI 目标 `jiade init --template bank --dir /tmp/e2e → up → seed → curl /healthz → curl /accounts`。

---

## 10. 错误处理

- **docker 缺失**：`up` 失败 + 安装提示；`init` 仅警告，照常脚手架。
- **daemon 没起**：`up` 探到 `docker info` 失败 → 「请先启动 Docker Desktop」。
- **目标目录非空**：`init` 拒绝 → 「用 `--force` 覆盖」（`--force` 即显式确认，符合「删除必须确认」原则）。
- **端口**：compose 固定 5432/8080；冲突由 docker compose 自身报错，`.env.example` 写明改法。
- **seed 失败**（库没起/连不上）：seed 二进制短暂重试后透出 pg 原始错误、非零退出；jiade 包一层「先 `jiade up` 再 `seed`」。
- **子进程 stderr**：`up`/`seed` 透传子进程 stderr 原文，不吞，退出码透传。
- **`--reset`/`--force`** 是所有破坏性操作的显式用户确认机制；无此二者，jiade 不删除任何东西。

---

## 11. Spec A 验收标准

1. jiade 仓自身：`go build ./...` 通过、`go test ./...` 全绿（不含 `templates/`，其见下条）。
2. `templates/bank/` 作为独立 module：拷到临时目录后 `go build ./... && go test ./...` 全绿（CI 单独步骤）。
3. `jiade list` 列出 `bank`。
4. `jiade init --template bank --dir /tmp/mybank` 在空目录产出完整工程；对非空目录拒绝；`--force` 覆盖。
5. `cd /tmp/mybank && jiade up && jiade seed` 后：`curl /healthz` 200；`GET /accounts/{no}` 返回 seed 出的真实账户；`GET /balance` 返回余额。
6. 同 Seed+Scale 两次 seed 产出完全一致（确定性单测通过）。
7. 复式记账不平的用例被单测拦截。
8. 生成物在未安装 `jiade` 的环境（仅 docker+go）下也能 `docker compose up` + `go run ./cmd/seed` 运行（自包含）。

---

## 12. Spec B 预告（不在本 spec）

- 补齐 bank 其余 6 服务（customer/payment/reward/risk/loan/wealth）及对应 schema（共 41 表）、fixture、只读 API。
- 多库 + FDW 跨库联邦。
- 完整多日切日引擎移植（逐日推进 + 三因子分布）。
- 写操作 HTTP 接口（过账/记账/冲正）。

---

## 13. 开放问题

- 无待决问题。Spec A 范围、契约、技术选型均已与用户确认；Jiade 自闭、不联动 SCV/Porto、无 `doctor` 命令。
