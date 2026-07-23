# jiade

[English README](README.md)

**jiade**（假的）生成「现实世界大工程的缩影」——**可运行的行业 Go 工程**：微服务、数据库、确定性种子数据、可用 API，一个不少。小到能装进脑子，真到能跑起来。

它脚手架的不是代码片段，而是**整套系统**：一份生产风格架构的可运行微缩版，可用于学习、演示、集成测试，或作为工具实验的基座。

## 生成物（`bank` 模板）

一个微缩银行核心系统——**7 个 Go 微服务 + 7 个 PostgreSQL 库**（单实例）。每个服务独占自己的库，跨域读取走 HTTP API：

| 服务 | 端口 | 库 | 内容 |
|------|------|----|------|
| core-banking | 18080 | core_db | 活期/定存账户、复式记账总账、逐日余额、写接口（过账/冲正） |
| customer | 18081 | cust_db | 客户信息、账户关系 |
| payment | 18082 | pay_db | 商户、转账、消费流水 |
| reward | 18083 | reward_db | 积分账户/流水、优惠券、活动 |
| risk | 18084 | risk_db | 风控规则、事件、黑名单 |
| loan | 18085 | loan_db | 借据、放款、月度还款、五级分类逾期、**逐日余额快照** |
| wealth | 18086 | wealth_db | 理财产品、**逐日净值游走**、持仓、申赎订单、每日利息 |

每个服务都是同一个四层纵切（`api → service → repo → domain`）。数据引擎要点：

- **确定性 fixture**：同 seed + scale → 完全相同的行。确定性 ID（无 UUID），逐日独立 rng（`seed + 偏移 + 日序`）。
- **两种数据形态**：三因子事件流（`趋势 × 季节 × 周期`——周末单量 < 工作日）与路径依赖的**逐日滚存快照**（账户余额、借据余额、净值游走）。
- **数据库按服务隔离**：每个服务只查自己的库，跨域数据通过 HTTP 获取（如 loan 调 customer 完成 `GET /api/v1/loan/accounts/{loan_no}/profile`）。
- **金额 int64 分，禁 float**；利率/净值/份额等非货币小数按 NUMERIC 文本直存。
- **生成物自包含**：离开 jiade 也能构建运行——只需 Docker 和 Go。

## 环境要求

- **Docker**（含 compose）——跑 postgres 与各服务
- **Go 1.22+**——构建 jiade、运行 seed

## 安装

```bash
go install github.com/projanvil/jiade/cmd/jiade@latest
```

或从源码构建：

```bash
git clone https://github.com/ProjAnvil/Jiade.git
cd Jiade
go build -o jiade ./cmd/jiade
```

## 快速开始

```bash
# 1. 生成工程（模板逐字拷贝）
jiade init --template bank --dir ./mybank

# 2. 起 postgres + 全部 7 个服务（并灌入数据）
cd mybank
jiade up      # docker compose up -d
jiade seed    # go run ./cmd/seed --scale=dev --reset

# 3. 试一试
curl localhost:18085/healthz                                          # loan
curl localhost:18086/healthz                                          # wealth
curl localhost:18085/api/v1/loan/accounts                             # 借据列表
curl localhost:18085/api/v1/loan/accounts/LN0000001/profile           # loan 调用 customer
curl 'localhost:18086/api/v1/wealth/nav?product_code=WP-FIX1'         # 逐日净值序列
curl 'localhost:18085/api/v1/loan/overdue?overdue_class=可疑'          # 五级分类逾期

# 4. 拆除
jiade down
```

生成物离开 jiade 也能跑：工程内 `make up` = postgres → seed → 全服务；`make seed` 重新灌数（`--reset` 重建 7 库）。

灌数规模：`--scale=dev`（约 1/4 量，默认）或 `--scale=full`。同 seed 重跑 `jiade seed` 产出完全相同的数据。

## 工作原理

- jiade 把模板打成 tar 内嵌（`internal/template/templates.tar`，改动后用 `go generate ./internal/template` 重打包），`init` 逐字拷出——零模板替换，`templates/bank/` 里是什么样，生成物就是什么样。
- `jiade up/down` 在目标目录包装 `docker compose up -d` / `down`（先探测 docker/compose/daemon）。
- `jiade seed` 运行生成物自带的灌数器：建 7 库 → 跑 7 套迁移 → 按依赖序灌各域（core → customer → payment → reward → risk → loan → wealth）。9 个幂等步骤。

## 仓库结构

```
cmd/jiade/           CLI 入口（cobra）
internal/cli/        list / init / up / down / seed 命令
internal/template/   内嵌模板 registry（tar 方案）
internal/docker/     docker/compose/daemon 探测
templates/bank/      bank 模板——独立 Go module（`module bank`）
docs/superpowers/    设计 spec 与实施计划
```

## 开发

```bash
# jiade 本体
go build ./... && go test ./...

# bank 模板（独立 module）
cd templates/bank
go build ./... && go test ./...
go test -tags=integration -p 1 ./...   # 需本机 15432 有 postgres（可用 DB_PORT 覆盖）

# 改动 templates/bank 后重新内嵌：
go generate ./internal/template
```

## License

[MIT](LICENSE)
