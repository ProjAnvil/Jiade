# jiade

[中文文档](README.zh-CN.md)

**jiade** (假的 — "simulated") generates **runnable microcosms of real-world industry systems** in Go: complete, self-contained projects with microservices, databases, deterministic seed data, and working APIs — small enough to hold in your head, real enough to run.

Think of it as scaffolding for *whole systems*, not just code: what you get is a working miniature of a production-style architecture, useful for learning, demos, integration testing, and as a substrate for tooling experiments.

## What you get (the `bank` template)

A miniature core-banking system — **7 Go microservices + 7 PostgreSQL databases** (single instance), wired together with `postgres_fdw` cross-database federation:

| Service | Port | Database | Contents |
|---------|------|----------|----------|
| core-banking | 18080 | core_db | Demand/fixed accounts, double-entry ledger, daily balances, write API (post/reverse) |
| customer | 18081 | cust_db | Customer info, account relationships (read-only + FDW join) |
| payment | 18082 | pay_db | Merchants, transfers, consumption txns (read-only + FDW join) |
| reward | 18083 | reward_db | Points accounts/txns, coupons, campaigns (read-only + FDW join) |
| risk | 18084 | risk_db | Risk rules, events, blacklist (read-only + FDW join) |
| loan | 18085 | loan_db | Loan accounts, disbursements, monthly repayment, 5-class overdue, **daily balance snapshots** |
| wealth | 18086 | wealth_db | Wealth products, **daily NAV walk**, holdings, orders, daily interest |

Every service follows the same four-layer vertical slice (`api → service → repo → domain`). Highlights of the data engine:

- **Deterministic fixtures**: same seed + scale → byte-identical rows. Reproducible IDs (no UUIDs), per-day RNG (`seed + offset + dayOrdinal`).
- **Two data shapes**: three-factor event streams (`trend × seasonal × cyclical` — weekend volume < weekday) and path-dependent **daily rolling snapshots** (account balances, loan balances, NAV walk).
- **Real cross-db federation**: each service queries its own database and joins `cust_db.cust_info` over `postgres_fdw` (e.g. `GET /api/v1/loan/accounts/{loan_no}/profile`).
- **Money is int64 cents**, never float. Rates/NAV/shares (non-monetary decimals) are stored as NUMERIC text.
- **Self-contained output**: the generated project builds and runs without jiade installed — only Docker and Go are needed.

## Requirements

- **Docker** (with compose) — runs postgres + the services
- **Go 1.22+** — builds jiade and runs the seeder

## Install

```bash
go install github.com/projanvil/jiade/cmd/jiade@latest
```

Or build from source:

```bash
git clone https://github.com/ProjAnvil/Jiade.git
cd Jiade
go build -o jiade ./cmd/jiade
```

## Quickstart

```bash
# 1. Generate a project (verbatim copy of the template)
jiade init --template bank --dir ./mybank

# 2. Start postgres + all 7 services (and seed the data)
cd mybank
jiade up      # docker compose up -d
jiade seed    # go run ./cmd/seed --scale=dev --reset

# 3. Probe it
curl localhost:18085/healthz                                          # loan
curl localhost:18086/healthz                                          # wealth
curl localhost:18085/api/v1/loan/accounts                             # loan list
curl localhost:18085/api/v1/loan/accounts/LN0000001/profile           # FDW: loan ⋈ customer
curl 'localhost:18086/api/v1/wealth/nav?product_code=WP-FIX1'         # daily NAV series
curl 'localhost:18085/api/v1/loan/overdue?overdue_class=可疑'          # 5-class overdue

# 4. Tear down
jiade down
```

The generated project also works without jiade: `make up` inside it runs postgres → seed → all services; `make seed` re-seeds (`--reset` rebuilds all 7 databases).

Seed scales: `--scale=dev` (~1/4 volume, default) or `--scale=full`. Re-running `jiade seed` with the same seed reproduces the exact same data.

## How it works

- jiade embeds the template as a tar (`internal/template/templates.tar`, rebuilt with `go generate ./internal/template`) and copies it out verbatim — zero templating/substitution, what you see in `templates/bank/` is what you get.
- `jiade up/down` wraps `docker compose up -d` / `down` in the target directory (with a docker/compose/daemon probe first).
- `jiade seed` runs the generated project's own seeder: create 7 databases → run 7 migrations → seed each domain in dependency order (core → customer → payment → reward → risk → loan → wealth) → set up FDW foreign tables. 10 idempotent steps.

## Repository layout

```
cmd/jiade/           CLI entrypoint (cobra)
internal/cli/        list / init / up / down / seed commands
internal/template/   embedded template registry (tar-based)
internal/docker/     docker/compose/daemon probe
templates/bank/      the bank template — a standalone Go module (`module bank`)
docs/superpowers/    design specs & implementation plans
```

## Development

```bash
# jiade itself
go build ./... && go test ./...

# the bank template (separate module)
cd templates/bank
go build ./... && go test ./...
go test -tags=integration -p 1 ./...   # needs a postgres on localhost:15432 (DB_PORT to override)

# after changing templates/bank, re-embed:
go generate ./internal/template
```

## License

[MIT](LICENSE)
