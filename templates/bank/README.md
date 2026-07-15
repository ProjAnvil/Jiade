# bank（jiade 模板：core-banking + customer + payment 纵切）

简化版银行核心系统——「现实世界大工程的缩影」。本工程由 `jiade init --template bank` 生成，**自包含**：离开 jiade 也可独立运行（仅需 docker + go）。

Spec B-1 扩展为 **3 服务 + 3 库 + FDW 跨库联邦**：

| 服务 | 端口 | 库 |
|------|------|----|
| core-banking | 8080 | core_db |
| customer | 8081 | cust_db |
| payment | 8082 | pay_db |

## 快速开始

```bash
make up       # docker compose up -d（postgres + core-banking + customer + payment）
make seed     # 建 3 库 → 建 3 库表 → 灌 fixture → setup_fdw（幂等：--reset）
```

三服务 healthz：

```bash
curl -sf localhost:8080/healthz                       # core-banking
curl -sf localhost:8081/healthz                       # customer
curl -sf localhost:8082/healthz                       # payment
```

core-banking 只读查询（Spec A）：

```bash
curl -sf localhost:8080/api/v1/accounts/D0000000001
curl -sf localhost:8080/api/v1/accounts/D0000000001/balance
```

**跨库 FDW JOIN 端点**（Spec B-1 核心：单条 SQL 跨 2~3 库，非应用层拼接）：

```bash
# customer 服务：cust_db.cust_account_rel JOIN core_db.demand_account（经 FDW 外部表）
curl -sf localhost:8081/api/v1/customers/C0000001/accounts

# payment 服务：pay_db.transfer_txn JOIN core_db.demand_account(×2) JOIN cust_db.cust_info(×2)
curl -sf localhost:8082/api/v1/payments/transfers/PT000000000001/parties
```

预期：`/accounts` 返回该客户在 core_db 的账户行；`/parties` 返回转账双方账号 + 户主客户姓名（跨 3 库联邦）。

## 架构

见 [ARCHITECTURE.md](ARCHITECTURE.md)。3 进程 + 3 库 + FDW 联邦；每服务分层 `api → service → repo → domain`，domain 零外部依赖。

## 金融不变量

- 金额用 int64 分表示，禁 float。
- 复式记账只在 core：过账强制 sum(借)==sum(贷)，不平回滚。customer/payment 无总账。
