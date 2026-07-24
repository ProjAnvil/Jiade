# jiade

[中文文档](README.zh-CN.md)

**jiade** (假的 — "simulated") generates **runnable microcosms of real-world industry systems** in Go: complete, self-contained projects with microservices, databases, deterministic seed data, and working APIs — small enough to hold in your head, real enough to run.

Think of it as scaffolding for *whole systems*, not just code: what you get is a working miniature of a production-style architecture, useful for learning, demos, integration testing, and as a substrate for tooling experiments.

## Built-in templates

Each template is a standalone Go module that ships its own README and ARCHITECTURE.md describing its services, ports, databases, and operations. Pick one and generate a runnable copy:

| Template | What it is | Docs |
|----------|------------|------|
| `bank` | Core banking microcosm — 7 Go services, 7 PostgreSQL databases, double-entry ledger, daily rolling balances. | [templates/bank/README.md](templates/bank/README.md) · [ARCHITECTURE.md](templates/bank/ARCHITECTURE.md) |
| `commerce` | Commerce backend microcosm — 6 Go services, 6 PostgreSQL databases, RabbitMQ saga, Traefik gateway. | [templates/commerce/README.md](templates/commerce/README.md) · [ARCHITECTURE.md](templates/commerce/ARCHITECTURE.md) |

```bash
jiade init --template <bank|commerce> --dir ./myproj
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

# 3. Probe a service healthz (ports and endpoints differ per template —
#    see the template's README).
curl localhost:18080/healthz                    # bank: core-banking
curl localhost:18100/api/v1/products?limit=1     # commerce: Traefik gateway

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
