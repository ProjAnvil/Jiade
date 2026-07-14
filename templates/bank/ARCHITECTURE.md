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
