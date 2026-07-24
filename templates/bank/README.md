# bank（jiade 模板：7 服务纵切——core-banking + customer + payment + reward + risk + loan + wealth）

简化版银行核心系统——「现实世界大工程的缩影」。本工程由 `jiade init --template bank` 生成，**自包含**：离开 jiade 也可独立运行（仅需 docker + go）。

本模板属于 [jiade](../../README.md) 项目；架构细节见 [ARCHITECTURE.md](ARCHITECTURE.md)。

工程包含 **7 服务 + 7 库 + 服务间 HTTP 调用 + 逐日滚存/三因子 fixture**。每个服务只访问自己的数据库：

| 服务 | 端口 | 库 | 内容 |
|------|------|----|------|
| core-banking | 18080 | core_db | 活期/定存账户、复式记账总账、逐日余额、写接口（过账/冲正） |
| customer | 18081 | cust_db | 客户信息、账户关系 |
| payment | 18082 | pay_db | 商户、转账、消费流水 |
| reward | 18083 | reward_db | 积分账户/流水、优惠券、活动 |
| risk | 18084 | risk_db | 风控规则、事件、黑名单 |
| loan | 18085 | loan_db | 借据、放款、月度还款、五级分类逾期、**逐日余额快照** |
| wealth | 18086 | wealth_db | 理财产品、**逐日净值游走**、持仓、申赎订单、每日利息 |

## 数据引擎要点

每个服务都是同一个四层纵切（`api → service → repo → domain`）。数据引擎要点：

- **确定性 fixture**：同 seed + scale → 完全相同的行。确定性 ID（无 UUID），逐日独立 rng（`seed + 偏移 + 日序`）。
- **两种数据形态**：三因子事件流（`趋势 × 季节 × 周期`——周末单量 < 工作日）与路径依赖的**逐日滚存快照**（账户余额、借据余额、净值游走）。
- **数据库按服务隔离**：每个服务只查自己的库，跨域数据通过 HTTP 获取（如 loan 调 customer 完成 `GET /api/v1/loan/accounts/{loan_no}/profile`）。
- **金额 int64 分，禁 float**；利率/净值/份额等非货币小数按 NUMERIC 文本直存。
- **生成物自包含**：离开 jiade 也能构建运行——只需 Docker 和 Go。

## 快速开始

```bash
make up       # docker compose up -d（postgres + 全部 7 服务）
make seed     # 建 7 库 → 建 7 库表 → 灌 7 域 fixture（9 步，幂等：--reset）
```

灌数规模：`--scale=dev`（约 1/4 量，默认）或 `--scale=full`。同 seed 重跑 `make seed`（或 `jiade seed`）产出完全相同的数据。`make seed` 走 `--reset`，会重建全部 7 库。

```bash
make seed                       # dev 规模（默认）
SCALE=full make seed            # full 规模
go test -tags=integration -p 1 ./...   # 集成测试，需本机 15432 有 postgres（DB_PORT 可覆盖）
```

七服务 healthz：

```bash
curl -sf localhost:18080/healthz                       # core-banking
curl -sf localhost:18081/healthz                       # customer
curl -sf localhost:18082/healthz                       # payment
curl -sf localhost:18083/healthz                       # reward
curl -sf localhost:18084/healthz                       # risk
curl -sf localhost:18085/healthz                       # loan
curl -sf localhost:18086/healthz                       # wealth
```

core-banking 只读查询（Spec A）：

```bash
curl -sf localhost:18080/api/v1/accounts/D0000000001
curl -sf localhost:18080/api/v1/accounts/D0000000001/balance
```

core-banking 记账/冲正写接口（Spec B-3；复式过账强制 sum(借)==sum(贷)，`LedgerService.Post` 已内部化，客户端只见业务意图）：

```bash
# 记账：存入 100 元（deposit / withdraw / transfer）
curl -sf -X POST localhost:18080/api/v1/txns \
  -H 'Content-Type: application/json' \
  -d '{"action":"deposit","account_no":"D0000000001","amount":"100.00","ccy":"CNY"}'
# → 201 {"voucher_no":"V...","biz_date":"...","txns":[{借/贷两条分录}]}

# 冲正：蓝冲（默认，改状态+回滚余额，不新增流水）
curl -sf -X POST 'localhost:18080/api/v1/vouchers/V.../reverse?mode=blue'
# → 200 {"voucher_no":"V...","mode":"blue","status":"reversed"}
# mode=red 走反向分录（新增反向流水，返回 reversed_voucher_no）
```

**跨服务聚合端点**（服务通过 HTTP API 协作，不跨库查询）：

```bash
# customer 查本库账户关系，再调用 core-banking 获取账户资料
curl -sf localhost:18081/api/v1/customers/C0000001/accounts

# payment 查本库转账，再调用 core-banking 和 customer 获取双方资料
curl -sf localhost:18082/api/v1/payments/transfers/PT000000000001/parties
```

预期：`/accounts` 返回该客户的 core 账户资料；`/parties` 返回转账双方账号 + 户主客户姓名。

loan/wealth 只读端点示例（Spec B-4b）：

```bash
curl -sf localhost:18085/api/v1/loan/accounts
curl -sf localhost:18085/api/v1/loan/accounts/{loan_no}/profile
curl -sf localhost:18086/api/v1/wealth/holdings/{holding_id}/profile
```

## 架构

见 [ARCHITECTURE.md](ARCHITECTURE.md)。7 进程 + 7 库；跨域读取走 HTTP API，每服务分层 `api → service → repo → domain`，domain 零外部依赖。

## 金融不变量

- 金额用 int64 分表示，禁 float。
- 复式记账只在 core：过账强制 sum(借)==sum(贷)，不平回滚——既护 seed 也护 B-3 运行时记账/冲正。customer/payment 无总账。
