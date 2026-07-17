# bank（jiade 模板：7 服务纵切——core-banking + customer + payment + reward + risk + loan + wealth）

简化版银行核心系统——「现实世界大工程的缩影」。本工程由 `jiade init --template bank` 生成，**自包含**：离开 jiade 也可独立运行（仅需 docker + go）。

Spec B-4b 扩展为 **7 服务 + 7 库 + FDW 跨库联邦 + 逐日滚存/三因子 fixture**：

| 服务 | 端口 | 库 |
|------|------|----|
| core-banking | 8080 | core_db |
| customer | 8081 | cust_db |
| payment | 8082 | pay_db |
| reward | 8083 | reward_db |
| risk | 8084 | risk_db |
| loan | 8085 | loan_db |
| wealth | 8086 | wealth_db |

## 快速开始

```bash
make up       # docker compose up -d（postgres + 全部 7 服务）
make seed     # 建 7 库 → 建 7 库表 → 灌 7 域 fixture → setup_fdw（10 步，幂等：--reset）
```

七服务 healthz：

```bash
curl -sf localhost:8080/healthz                       # core-banking
curl -sf localhost:8081/healthz                       # customer
curl -sf localhost:8082/healthz                       # payment
curl -sf localhost:8083/healthz                       # reward
curl -sf localhost:8084/healthz                       # risk
curl -sf localhost:8085/healthz                       # loan
curl -sf localhost:8086/healthz                       # wealth
```

core-banking 只读查询（Spec A）：

```bash
curl -sf localhost:8080/api/v1/accounts/D0000000001
curl -sf localhost:8080/api/v1/accounts/D0000000001/balance
```

core-banking 记账/冲正写接口（Spec B-3；复式过账强制 sum(借)==sum(贷)，`LedgerService.Post` 已内部化，客户端只见业务意图）：

```bash
# 记账：存入 100 元（deposit / withdraw / transfer）
curl -sf -X POST localhost:8080/api/v1/txns \
  -H 'Content-Type: application/json' \
  -d '{"action":"deposit","account_no":"D0000000001","amount":"100.00","ccy":"CNY"}'
# → 201 {"voucher_no":"V...","biz_date":"...","txns":[{借/贷两条分录}]}

# 冲正：蓝冲（默认，改状态+回滚余额，不新增流水）
curl -sf -X POST 'localhost:8080/api/v1/vouchers/V.../reverse?mode=blue'
# → 200 {"voucher_no":"V...","mode":"blue","status":"reversed"}
# mode=red 走反向分录（新增反向流水，返回 reversed_voucher_no）
```

**跨库 FDW JOIN 端点**（Spec B-1 核心：单条 SQL 跨 2~3 库，非应用层拼接）：

```bash
# customer 服务：cust_db.cust_account_rel JOIN core_db.demand_account（经 FDW 外部表）
curl -sf localhost:8081/api/v1/customers/C0000001/accounts

# payment 服务：pay_db.transfer_txn JOIN core_db.demand_account(×2) JOIN cust_db.cust_info(×2)
curl -sf localhost:8082/api/v1/payments/transfers/PT000000000001/parties
```

预期：`/accounts` 返回该客户在 core_db 的账户行；`/parties` 返回转账双方账号 + 户主客户姓名（跨 3 库联邦）。

loan/wealth 只读端点示例（Spec B-4b）：

```bash
curl -sf localhost:8085/api/v1/loan/accounts
curl -sf localhost:8085/api/v1/loan/accounts/{loan_no}/profile
curl -sf localhost:8086/api/v1/wealth/holdings/{holding_id}/profile
```

## 架构

见 [ARCHITECTURE.md](ARCHITECTURE.md)。7 进程 + 7 库 + FDW 联邦；每服务分层 `api → service → repo → domain`，domain 零外部依赖。

## 金融不变量

- 金额用 int64 分表示，禁 float。
- 复式记账只在 core：过账强制 sum(借)==sum(贷)，不平回滚——既护 seed 也护 B-3 运行时记账/冲正。customer/payment 无总账。
