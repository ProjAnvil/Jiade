# jiade

[English README](README.md)

**jiade**（假的）生成「现实世界大工程的缩影」——**可运行的行业 Go 工程**：微服务、数据库、确定性种子数据、可用 API，一个不少。小到能装进脑子，真到能跑起来。

它脚手架的不是代码片段，而是**整套系统**：一份生产风格架构的可运行微缩版，可用于学习、演示、集成测试，或作为工具实验的基座。

## 内置模板

每个模板都是独立的 Go module，自带 README 与 ARCHITECTURE.md，说明其服务、端口、数据库与操作方式。选一个模板，生成可运行副本：

| 模板 | 是什么 | 文档 |
|------|--------|------|
| `bank` | 银行核心系统缩影——7 个 Go 服务、7 个 PostgreSQL 库、复式记账总账、逐日滚存余额。 | [templates/bank/README.md](templates/bank/README.md) · [ARCHITECTURE.md](templates/bank/ARCHITECTURE.md) |
| `commerce` | 电商后端缩影——6 个 Go 服务、6 个 PostgreSQL 库、RabbitMQ saga、Traefik 网关。 | [templates/commerce/README.md](templates/commerce/README.md) · [ARCHITECTURE.md](templates/commerce/ARCHITECTURE.md) |

```bash
jiade init --template <bank|commerce> --dir ./myproj
cd myproj && make up
```

详细架构（服务/端口表、数据引擎说明、各模板操作指南）位于各模板目录内——根 README 只做简要总览。

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
# 1. 生成工程（模板逐字拷贝）。
jiade init --template bank --dir ./mybank     # 或：--template commerce --dir ./myshop

# 2. 起 postgres + 全部服务并灌入数据。
cd mybank
jiade up      # docker compose up -d
jiade seed    # go run ./cmd/seed --scale=dev --reset

# 3. 探一下 healthz（端口与端点因模板而异——见各模板 README）。
curl localhost:18080/healthz                    # bank：core-banking
curl localhost:18100/api/v1/products?limit=1     # commerce：Traefik 网关

# 4. 拆除。
jiade down
```

生成物离开 jiade 也能跑：工程内 `make up` 一步到位（postgres → seed → 全服务），`make seed` 重新灌数。灌数规模与探查端点见各模板 README。

## 工作原理

- jiade 把所有内置模板打成 tar 内嵌（`internal/template/templates.tar`，改动后用 `go generate ./internal/template` 重打包），`init` 逐字拷出——零模板替换。
- `jiade up/down` 在目标目录包装 `docker compose up -d` / `down`（先探测 docker/compose/daemon）。
- `jiade seed` 运行生成物自带的灌数器：建库 → 跑迁移 → 按依赖序灌各域。各步骤幂等。

## 仓库结构

```
cmd/jiade/           CLI 入口（cobra）
internal/cli/        list / init / up / down / seed 命令
internal/template/   内嵌模板 registry（tar 方案）
internal/docker/     docker/compose/daemon 探测
templates/bank/      bank 模板——独立 Go module（`module bank`）
templates/commerce/  commerce 模板——独立 Go module（`module commerce`）
docs/superpowers/    设计 spec 与实施计划
```

## 开发

```bash
# jiade 本体
go build ./... && go test ./...

# 某个模板（独立 module，以 bank 为例）
cd templates/bank
go build ./... && go test ./...
go test -tags=integration -p 1 ./...   # 需本机 15432 有 postgres（可用 DB_PORT 覆盖）

# 改动任一模板后重新内嵌：
go generate ./internal/template
```

## License

[MIT](LICENSE)
