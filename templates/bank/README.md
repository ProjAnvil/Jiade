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
