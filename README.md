# jiade

生成「现实世界大工程的缩影」——可运行的行业 Go 工程。

## 安装

```bash
go install github.com/projanvil/jiade/cmd/jiade@latest
```

## 快速开始

```bash
jiade init --template bank --dir ./mybank
cd mybank
jiade up      # docker compose up -d
jiade seed    # go run ./cmd/seed --scale=dev --reset
```

生成物自包含：未安装 jiade 也可用 `make up && make seed`（见模板内 Makefile）。

Jiade 自闭：不与 SCV/Porto 联动，无 doctor 命令。
