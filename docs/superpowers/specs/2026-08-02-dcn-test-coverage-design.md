# DCN 模板测试补齐 · 设计文档

> 在 `feat/dcn-production-realism` 分支的生产化增强之上，补齐各微服务 Go 单测与外部发起的 Go 集成测试。本文档是本轮改动的实现依据（spec）。
> 命名约束沿用：任何文件中不得出现特定银行机构名称；架构统一称为「DCN 架构」。

## 1. 背景与目标

生产化增强终审确认：dcn 模板功能完整（verify 8 关全过），但 Go 测试覆盖存在结构性缺口——`rmb`（协调器状态机，全模板最复杂逻辑）、`adm`、`gns/server.go` 三个服务零 Go 测试，`dcnapp` 仅有纯函数测试；外部集成测试只有 bash `verify.sh`，而 bank/commerce 模板均有 Go `-tags=integration` 集成测试惯例。

目标：

1. 上述四个服务的核心行为补齐 Go 单测（零新依赖）；
2. 新增 Go 集成测试包，从宿主机对运行中的栈发请求断言（`//go:build integration`，对齐 commerce 惯例）；
3. `verify.sh` 保留为端到端冒烟，不改动其逻辑。

## 2. 方案取舍（已决策）

| 候选 | 说明 | 结论 |
| ---- | ---- | ---- |
| **A. 单测补齐 + Go 集成测试** | 对齐 bank/commerce 惯例；断言能力强、可按服务拆分 | **采用（用户选定）** |
| B. 只补 Go 单测 | 外部集成维持 bash verify.sh | 否决：用户要求 Go 集成测试 |
| C. A + verify.sh 扩故障场景 | 再加批量中途杀调度器等场景 | 否决：YAGNI，verify.sh 现状已覆盖核心故障语义 |
| 单测引 testify/sqlmock/miniredis | 写法更省事 | 否决：全局约束「新依赖仅限 client_golang」（本轮沿用），且 batch 已验证 fake driver + httptest 手法可行 |
| gns Locate 缓存路径单测 | 需 Redis 替身 | 否决：不引 miniredis，缓存路径由集成测试覆盖 |

## 3. 单测补齐（零新依赖）

测试替身手法：进程内实现 `database/sql/driver` 的 fake driver（batch/scheduler_test.go 已验证）+ `net/http/httptest` 假上游。在 `internal/platform/` 下新增仅供测试使用的 `sqltest` 包（`_test.go` 无法跨包共享，故独立成包但仅供各包测试引用；包文档注明「测试专用」）。

| 服务 | 覆盖点 |
| ---- | ------ |
| `rmb`（重点） | `register` txId 幂等返回；`handleReceipt`+`advance`：全 DONE→COMMITTED；步骤 FAILED→逆序补建 COMPENSATE 步骤并置 PENDING；补偿齐→COMPENSATED；迟到 DONE 回执→重开 PROCESSING 再补偿 |
| `adm` | `handleEvent` 幂等（重复事件 uk_event 去重、global_balance 只加一次）；`handleReconcile` 汇总与 consistent 判定（httptest 假 GNS/假单元 + fake driver） |
| `gns` | `handleOpenAccount`：requestId 幂等命中返回首次结果；目标 DCN 建户失败回滚路由行（httptest 假 DCN + fake driver） |
| `dcnapp` | `handleTransfer` 三分支路由决策（本单元 localTransfer / 跨单元提交 RMB / 透明转发——httptest 假 GNS + 假 RMB + fake driver）；`handleCreateAccount` 重复开户返回 exists |

约束：不修改被测代码的公开行为；如确需为可测性做小重构（例如把 SQL 字符串常量化），在实现计划中显式列出。

## 4. Go 集成测试（`//go:build integration`）

新建独立包 `templates/dcn/test/integration/`（避免污染内部包，也避免与各 internal 包循环依赖）。惯例对齐 commerce：

- 端点经环境变量覆盖（`DCN_BASE_GNS` 等），默认 localhost 各宿主机端口（18080–18099、18070）；
- 栈不可达即 `t.Skip`（与 commerce 的 `TEST_DATABASE_URL` 未设置即 Skip 同义）；
- 测试自备数据：经 GNS 开户（requestId 带测试名，幂等键天然防重复），断言用前后差值，不依赖 seed 状态、不与 verify.sh 互斥（可在 seed 过的栈上跑）。

文件与服务一一对应：

| 文件 | 断言 |
| ---- | ---- |
| `gns_test.go` | locate 未开户 404 → 开户 201 → locate 命中同 DCN → 同 requestId 重复开户返回同 accountId |
| `dcnapp_test.go` | 两账户本单元转账（直连单元端口）：余额差值精确、响应含 txId |
| `rmb_test.go` | 经 DCN 发起跨单元转账 → RMB `GET /transactions/{txId}` COMMITTED 且两步骤 DONE → 同 txId 重复 POST /transactions 返回同状态不重复入账 |
| `adm_test.go` | 本单元转账后等 ≤5s：`/reconcile` consistent=true 且 perDcn 覆盖全部 ACTIVE 单元 |
| `batch_test.go` | 经网关 `POST /batch/jobs/interest {bizDate: 昨日日期}`（避免与 verify gate 8 的当日任务冲突）→ SUCCEEDED → 重复 POST 幂等（totalInterest 不变、余额无二次入账） |
| `gateway_test.go` | 经 18070：`/gns/locate`、`/dcn/transfer`（LB 落任意单元仍能完成本单元转账，验证接入层透明转发） |
| `metrics_test.go` | 先向每个服务发一条已埋点请求（如 gns `/locate`、adm `/report/summary`、rmb `/transactions/{id}`、batch `/jobs/{date}`、dcnapp `/accounts/{id}/balance`、console `/api/targets`——`CounterVec` 首次计数才产出序列），再断言该服务 `/metrics` 200 且含 `http_requests_total{service="<name>"`；`/healthz` 未埋点，不能用于此目的 |
| `console_test.go` | `/api/targets`、`/api/containers` 200 且返回合法 JSON |

工程接线：

- `templates/dcn/Makefile` 加 `integration-test`：`go test -tags=integration -p 1 ./test/integration/...`（`-p 1` 串行，防并发扰动断言）；
- CI `.github/workflows/ci.yml` 的 `dcn-e2e` job 在 `make verify` 后加一步 `cd templates/dcn && make integration-test`（栈在跑，不会 skip；失败时同样有日志捕获）；
- README（EN/ZH）与 ARCHITECTURE.md 注明集成测试用法与 Skip 语义；
- 改动后 `go generate ./internal/template` 重新打包。

## 5. 交付约束

- 全局约束沿用：无机构名；注释中文、标识符英文；零新 Go 依赖；每任务 `go build ./... && go test ./...` 绿；任务级 commit。
- 单测不得依赖 docker 栈运行（`go test ./...` 在无栈环境必须全绿）；集成测试仅在 `-tags=integration` 下编译。
- verify.sh / topology.sh 逻辑零改动（CI 只加一步）。
