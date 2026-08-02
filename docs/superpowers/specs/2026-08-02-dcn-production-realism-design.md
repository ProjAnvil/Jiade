# DCN 模板生产化增强 · 设计文档

> 在现有 `templates/dcn`（设计见 [2026-08-02-dcn-template-design.md](2026-08-02-dcn-template-design.md)）基础上做「贴近实际生产」的增强。本文档是本轮改动的实现依据（spec）。
> 命名约束沿用：任何文件中不得出现特定银行机构名称；架构统一称为「DCN 架构」。

## 1. 背景与目标

`dcn` 模板当前的事务协调内核（RMB 补偿状态机、迟到回执再补偿、journal 幂等）已达设计预期，但外围生产形态偏简单，与 `bank`（271 文件）/`commerce`（145 文件）相比差距明显：无统一接入层、零可观测性、业务面只有开户+转账、种子数据仅 6 个账户、号段管理有扩无收。

本轮目标（已与用户对齐的范围）：

1. **日终结息批量**：核心银行最经典的日终场景——按单元并行跑批、全局调度、ADM 链路核对；
2. **Traefik 统一接入层**：客户端不再按端口直连单元，体现「接入层不感知账户、单元内再定位」的真实形态；
3. **真实规模 seed**：中文姓名词汇表 + 确定性随机余额，dev/full 两档规模；
4. **可观测性全家桶**：Prometheus（docker_sd 自动发现）+ Grafana（变量驱动仪表盘）+ 自建 console（拓扑 + 状态墙），RED 方法指标模型 + 基础设施 exporter。

明确不做（本轮范围外）：单元排水/数据迁移（DRAINING 流程）、K8s 部署清单、安全合规能力、分布式链路追踪（OTel/Jaeger）。

## 2. 方案取舍（已决策）

| 候选 | 说明 | 结论 |
| ---- | ---- | ---- |
| 批量调度：ADM 兼任 | 零新增服务，贴合「全局场景归 ADM」 | 否决：用户选择更贴近生产的独立调度服务 |
| **批量调度：独立 batch-scheduler 服务** | 独立 Go 服务 + 独立 `batch_db`，仿真生产独立批量调度平台 | **采用** |
| 批量调度：脚本驱动 | Makefile 循环 curl | 否决：失去「全局调度」架构表达 |
| 观测：自建轻量 console 单一方案 | 无外部依赖 | 否决：用户要求完整观测栈 |
| **观测：Prometheus + Grafana + console** | docker_sd 自动发现；Grafana 用变量驱动通用仪表盘而非逐服务手写 JSON；console 负责拓扑视角 | **采用** |
| 单元排水（DRAINING） | 新开户切流 / 存量数据迁移 | 均否决：本轮不做 |

调研依据（业界做法对照）：

- 服务发现：Prometheus 官方 docker_sd / dockerswarm_sd 文档与社区实践均为「容器打 label → Docker API 自动发现」；K8s 生态对应 ServiceMonitor label selector；
- 指标模型：RED 方法（Rate / Errors / Duration，Tom Wilkie）是微服务监控事实标准，Grafana 官方仪表盘最佳实践推荐「每服务一行，左 rate/error、右 duration」布局；
- 仪表盘免维护：`label_values(metric, service)` 模板变量 + `service=~"$service"` 正则匹配，一个仪表盘适配任意数量服务；
- 基础设施观测：cAdvisor/exporter 模式（mysqld-exporter、redis-exporter、RabbitMQ 内置 prometheus 插件）。

## 3. 组件与拓扑变化

新增 6 个平台容器（全部挂 `global-net`）：

| 组件 | 镜像/构建 | 端口 | 职责 |
| ---- | ---- | ---- | ---- |
| `batch-scheduler` | 源码构建（`cmd/batch-scheduler`） | 18092 | 日终批量调度：发起结息、归集分单元结果、幂等重跑控制 |
| `batch-db` | mysql:8.0 | 13313 | batch-scheduler 独立库（沿用「每组件一库」惯例） |
| `traefik` | traefik:v3 | 18070（入口）/ 18071（dashboard） | 统一接入层，docker provider + labels 路由 |
| `prometheus` | prom/prometheus | 19090 | 指标采集，docker_sd 自动发现（挂 docker socket） |
| `grafana` | grafana/grafana | 13000 | 可视化，YAML provisioning 数据源 + 通用仪表盘 |
| `console` | 源码构建（`cmd/console`） | 18099 | 自建观测页：拓扑视图 + 状态墙 + RPS 曲线 |

基础设施 exporter：`mysqld-exporter`（单实例 `/probe` 多 target 抓全部 7 个 MySQL 库；需跨 global-net/idc1/idc2 三网络部署以触达各库）、`redis-exporter`（抓 gns-redis）、RabbitMQ 启用内置 `rabbitmq_prometheus` 插件（15692）。cAdvisor 可选（默认不启用，避免容器数膨胀；README 注明启用方法）。

各服务原有直暴露端口全部保留（README 注明网关为「真实路径」，直暴露端口用于教学观察与 verify）。

## 4. batch-scheduler 设计

### 4.1 数据模型（batch_db）

```sql
CREATE TABLE batch_job (
  biz_date    VARCHAR(10) PRIMARY KEY,     -- YYYY-MM-DD
  type        VARCHAR(32) NOT NULL,        -- INTEREST（本期唯一类型）
  status      VARCHAR(16) NOT NULL,        -- RUNNING/SUCCEEDED/FAILED
  total_interest DECIMAL(18,2) NOT NULL DEFAULT 0,
  created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  finished_at TIMESTAMP NULL
);
CREATE TABLE batch_unit_result (
  biz_date  VARCHAR(10) NOT NULL,
  dcn       VARCHAR(16) NOT NULL,
  accounts  INT NOT NULL,
  interest  DECIMAL(18,2) NOT NULL,
  status    VARCHAR(16) NOT NULL,          -- DONE/FAILED
  error     VARCHAR(512) NULL,
  PRIMARY KEY (biz_date, dcn)
);
```

### 4.2 接口与流程

- `POST /jobs/interest {bizDate}`：按 `biz_date` 幂等——状态为 `RUNNING`/`SUCCEEDED` 时直接返回当前状态（不重跑）；状态为 `FAILED` 时允许重试，只重跑失败单元，已成功单元以单元侧 journal 幂等兜底不重复入账。执行：经 GNS `/routes` 取 ACTIVE 单元 → 并发 `POST <endpoint>/internal/batch/interest {bizDate}` → 逐单元落 `batch_unit_result` → 全部成功则汇总 `total_interest` 置 `SUCCEEDED`；任一单元失败置 `FAILED`。
- `GET /jobs/{bizDate}`：任务状态 + 分单元明细。
- 同步等待窗口：HTTP 调用阻塞至终态（上限 30s），超时返回 RUNNING 由调用方轮询。

## 5. 单元侧结息（dcn-app）

`POST /internal/batch/interest {bizDate}`：

- 遍历本单元全部账户，**逐账户独立本地事务**（仿真生产批量按笔提交，非单一大事务）；
- journal 幂等键 `interest-<bizDate>-<accountId>`（`uk_tx_acct` 兜底，重跑/重复触发安全）；
- 日利率走 env `INTEREST_DAILY_RATE`（默认 `0.0001`）；利息 = 余额 × 日利率，按 2 位小数 half-even 取舍，为 0 跳过；
- 每笔入账复用现有 `publishEvent`（txId = journal 幂等键）上报 ADM，全局镜像自动跟进，`/reconcile` 现成兜底；
- 响应 `{dcn, accounts, totalInterest}`（accounts 为实际入账笔数）。

## 6. Traefik 接入层

- docker provider + 服务 labels 声明路由，无静态配置文件；
- `/dcn/*`：剥前缀后轮询（LB）转发到 dcn01/02/03-app——接入层不感知账户，单元内由现有 `forward()` 透明定位到正确单元，天然适配；
- 管理面路由：`/gns/*`→gns、`/adm/*`→adm、`/rmb/*`→rmb-coordinator、`/batch/*`→batch-scheduler，均剥前缀；
- dcn04（expansion profile）打同样 label，扩容上线即自动加入 LB 池；
- traefik 自身也打 `prometheus.scrape=true` label（自带 `/metrics`），接入层流量可观测。

## 7. 可观测性

### 7.1 埋点

- 新增 `internal/platform/metrics`（封装 `prometheus/client_golang`）：`httpx` 中间件统一采集 `http_requests_total{service,path,code}` 与 `http_request_duration_seconds` 直方图（RED 三要素）；`service` 标签由 env `DCN_ID`/服务名注入；
- RMB 协调器增加 `rmb_tx_total{status}` 计数器（COMMITTED/COMPENSATED/FAILED）；
- 各服务暴露 `GET /metrics`（不经过限流中间件）。

### 7.2 采集与发现

- Prometheus 两类采集并存：① `docker_sd_configs`（挂 `/var/run/docker.sock`）按 label（`prometheus.scrape=true` / `prometheus.port`）自动发现全部 Go 服务、traefik、redis-exporter、rabbitmq(15692)——dcn04 扩容自动入监控；② MySQL 全部七库走 mysqld-exporter 的 `/probe` 多目标模式，静态 job 列出库地址（库拓扑固定，不随业务扩容）；
- scrape 间隔 5s（仿真场景取短间隔便于演示）。

### 7.3 Grafana

- YAML provisioning：Prometheus 数据源 + 一个通用仪表盘；
- 仪表盘按 RED 布局：模板变量 `service`（`label_values(http_requests_total, service)`，multi + All），面板含 RPS、错误率、P99 延迟、RMB 事务状态、容器/数据库指标区；
- 新服务被 Prometheus 发现后下拉自动出现，无需改任何配置。

### 7.4 console 服务

- 内嵌纯 HTML/JS 单页（无前端构建链，`embed` 打包）；
- 数据源一：Prometheus HTTP API——`/api/v1/targets` 渲染服务清单与 up/down 状态墙，`rate(http_requests_total[1m])` 画各服务 RPS 迷你曲线；
- 数据源二：Docker API（挂 socket，只读）——列出全部容器（含 MySQL/Redis/RabbitMQ）healthcheck 状态；
- 页面含按 IDC 分组的拓扑视图（静态布局 + 实时状态点），这是 Grafana 不擅长的视角。

## 8. 真实规模 seed

- 参照 bank/commerce 生成器模式：中文姓名词汇表（`internal/seed/vocabulary.go` 同款思路）+ 确定性随机（固定 seed 的 `math/rand`）初始余额（100.00–100000.00 区间）；
- `--scale=dev`：每单元 50 户（150 户总计）；`--scale=full`：每单元 2000 户（6000 户总计）；
- 沿用 `seed-<scale>-<seg>-<i>` 幂等键，重灌不产生重复账户；
- 固定账户 1001/1002/2001/2002/3001/3002 保持确定性余额 1000.00（verify 与 README 示例依赖），其余为生成数据；
- `--reset` 语义不变（仅覆盖基础三单元拓扑）。

## 9. verify 扩为 8 关

| # | 用例 | 变化 |
|---|------|------|
| 1 | DCN 内转账 | 改走网关 `localhost:18070/dcn/transfer`（顺带验证接入层路由与 LB） |
| 2 | 跨 DCN 转账 | 改走网关 |
| 3–7 | 爆炸半径 / 崩溃恢复 / 幂等 / 在线扩容 / ADM 汇总 | 不变 |
| 8 | 日终批量（新） | `POST /batch/jobs/interest`（经网关）→ 断言各单元利息合计 = 调度器归集值 → 同 bizDate 重复触发断言幂等（总额不变）→ 等 3s 后 `/reconcile` consistent |

`make topology-test` 增补断言：traefik/prometheus/grafana/console/batch-* 在 global-net。

## 10. 测试与文档

- Go 单测：结息计算与取舍（`internal/dcnapp/interest_test.go`）、调度器幂等注册与失败重跑（`internal/batch/*_test.go`）、metrics 中间件计数；
- 文档：README（EN/ZH）拓扑图、组件表、端口表、快速上手（make up 后访问 console/grafana）；ARCHITECTURE.md 新增批量时序图（调度器→单元→ADM 核对）、观测体系一节、「与生产差异」清单更新（新增项：生产批量调度有依赖编排与断点续跑，本仿真仅单任务类型；生产观测含告警与日志链路，本仿真仅指标）。

## 11. 交付约束

- 模板仍是自包含 Go module，`make up && make seed && make verify` 一条链路跑通；
- 改动完成后 `go generate ./internal/template` 重新打包 `templates.tar`；
- 新依赖仅限 `prometheus/client_golang`（go.mod 变更最小化）；
- 容器总数从 11 → 19（基础拓扑 11 + batch 2 + traefik 1 + prometheus/grafana 2 + console 1 + exporter 2）；docker compose 资源占用需在 README 注明（建议 Docker Desktop ≥ 4GB 内存）。
