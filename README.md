# jiade

[中文文档](README.zh-CN.md)

**jiade** (假的 — "simulated") generates **runnable microcosms of real-world industry systems** in Go: complete, self-contained projects with microservices, databases, deterministic seed data, and working APIs — small enough to hold in your head, real enough to run.

Think of it as scaffolding for *whole systems*, not just code: what you get is a working miniature of a production-style architecture, useful for learning, demos, integration testing, and as a substrate for tooling experiments.

## Built-in templates

Each template is a standalone Go module that ships its own README and ARCHITECTURE.md describing its services, ports, databases, and operations. Pick one and generate a runnable copy:

| Template | What it is | Docs |
|----------|------------|------|
| `bank` | Core banking microcosm — 7 Go services, 7 dedicated PostgreSQL databases, internal gRPC reads, RabbitMQ commands/events, Traefik gateway (:18000), double-entry ledger, daily rolling balances, **durable payment-transfer saga** (risk→hold→transfer forward, reverse-order compensation, operator admin gRPC), **full observability stack** (OTel/Jaeger/Prometheus/Grafana + 5 alerts), and **dev/prod Kubernetes overlays** (dev = stateful, prod = external state + SecretProviderClass). | [templates/bank/README.md](templates/bank/README.md) · [ARCHITECTURE.md](templates/bank/ARCHITECTURE.md) |
| `commerce` | Commerce backend microcosm — 6 Go services, 6 PostgreSQL databases, RabbitMQ saga, Traefik gateway. | [templates/commerce/README.md](templates/commerce/README.md) · [ARCHITECTURE.md](templates/commerce/ARCHITECTURE.md) |
| `bank_dcn` | DCN unit-architecture simulation — GNS global routing (Redis+MySQL), RMB reliable message bus with transaction coordinator (RabbitMQ), 3 self-contained DCN units across 2 simulated IDCs, ADM global summary & reconcile, 8-gate verify suite (blast radius, crash recovery, idempotency, online expansion, day-end batch), Traefik unified gateway, Prometheus/Grafana/console observability stack. | [templates/bank_dcn/README.md](templates/bank_dcn/README.md) · [ARCHITECTURE.md](templates/bank_dcn/ARCHITECTURE.md) |

```bash
jiade init --template <bank|commerce|bank_dcn> --dir ./myproj
cd myproj && make up
```

The detailed architecture (service/port tables, data-engine notes, per-template operations) lives in each template's own directory — the root README stays a brief overview.

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
# 1. Generate a project (verbatim copy of the template).
jiade init --template bank --dir ./mybank     # or: --template commerce --dir ./myshop

# 2. Bring up postgres + all services and seed the data.
cd mybank
jiade up      # docker compose up -d
jiade seed    # go run ./cmd/seed --scale=dev --reset

# 3. Probe a public REST endpoint via the gateway (ports differ per template —
#    see the template's README).
curl localhost:18000/api/v1/accounts/D0000000001  # bank: Traefik gateway
curl localhost:18100/api/v1/products?limit=1       # commerce: Traefik gateway

# 4. Tear down.
jiade down
```

The generated project also works without jiade installed: `make up` inside it runs the full bring-up (postgres → seed → services), and `make seed` re-seeds. Seed scales and probe endpoints are documented in each template's README.

## How it works

- jiade embeds all built-in templates as a tar (`internal/template/templates.tar`, rebuilt with `go generate ./internal/template`) and copies the selected one out verbatim — no template substitution.
- `jiade up/down` wraps `docker compose up -d` / `down` in the target directory (with a docker/compose/daemon probe first).
- `jiade seed` runs the generated project's own seeder: create databases → run migrations → seed each domain in dependency order. Steps are idempotent.

## Repository layout

```
cmd/jiade/           CLI entrypoint (cobra)
internal/cli/        list / init / up / down / seed commands
internal/template/   embedded template registry (tar-based)
internal/docker/     docker/compose/daemon probe
templates/bank/      the bank template — a standalone Go module (`module bank`)
templates/commerce/  the commerce template — a standalone Go module (`module commerce`)
templates/bank_dcn/  the bank_dcn template — a standalone Go module (`module bank_dcn`)
docs/superpowers/    design specs & implementation plans
```

## Development

```bash
# jiade itself
go build ./... && go test ./...

# a template (separate module, e.g. bank)
cd templates/bank
go build ./... && go test ./...
go test -tags=integration -p 1 ./...   # needs a postgres on localhost:15432 (DB_PORT to override)

# after changing any template, re-embed:
go generate ./internal/template
```

## License

[MIT](LICENSE)
