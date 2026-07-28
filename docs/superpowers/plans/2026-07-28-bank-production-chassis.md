# Bank Production Chassis Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give bank a scalable Compose/Kubernetes chassis with dedicated databases, RabbitMQ, Traefik, internal gRPC discovery, hardened runtime defaults, and health-aware lifecycle management.

**Architecture:** External REST traffic enters through one Traefik gateway. Internal synchronous reads use generated gRPC contracts over DNS round-robin, while a domain-neutral RabbitMQ/Outbox foundation supplies the subsequent durable workflow plan; each service retains its own data store.

**Tech Stack:** Go 1.22, gRPC Go 1.67.1, Protobuf Go 1.35.1, RabbitMQ AMQP client 1.10.0, PostgreSQL 16, Traefik 3, Docker Compose, Kubernetes/Kustomize.

## Global Constraints

- External APIs remain REST; service-to-service HTTP is removed from bank.
- Container REST port is `8080`; internal gRPC port is `9090`.
- Default replicas: core-banking 2, customer 1, payment 2, risk 2, reward 1, loan 1, wealth 1.
- Every service owns one PostgreSQL container and volume.
- Traefik is the only published application entry point.
- The local default targets a 6-8 GB memory budget and does not claim stateful HA.
- Generated Protobuf Go files are committed.
- Every task ends with focused tests and a commit.

---

## File Map

- `templates/bank/proto/bank/*/v1/*.proto`: internal query contracts.
- `templates/bank/gen/bank/*/v1/*.pb.go`: committed generated contracts.
- `templates/bank/internal/platform/grpcx`: DNS client, health server, and shutdown.
- `templates/bank/internal/platform/httpx`: REST health, readiness, metrics, and middleware.
- `templates/bank/internal/platform/messaging`: envelope and broker primitives.
- `templates/bank/compose.yaml`: scalable production-shaped local topology.
- `templates/bank/deploy/traefik/traefik.yaml`: public REST gateway.
- `templates/bank/deploy/rabbitmq/definitions.json`: bank exchanges and queues.
- `templates/bank/deploy/k8s`: base application, service, availability, and gateway resources.

### Task 1: Add Internal Protobuf Contracts

**Files:**
- Create: `templates/bank/buf.yaml`
- Create: `templates/bank/buf.gen.yaml`
- Create: `templates/bank/proto/bank/customer/v1/customer_query.proto`
- Create: `templates/bank/proto/bank/core/v1/account_query.proto`
- Create: generated files under `templates/bank/gen/bank/customer/v1`
- Create: generated files under `templates/bank/gen/bank/core/v1`
- Modify: `templates/bank/go.mod`
- Modify: `templates/bank/go.sum`
- Modify: `templates/bank/Makefile`
- Test: `templates/bank/internal/platform/grpcx/contracts_test.go`

**Interfaces:**
- Produces: `customerv1.CustomerQueryServiceClient.GetCustomer`.
- Produces: `corev1.AccountQueryServiceClient.GetAccount`.

- [ ] **Step 1: Write contract compilation tests**

```go
func TestQueryContractsExposeRequiredMethods(t *testing.T) {
	var customer customerv1.CustomerQueryServiceClient
	var account corev1.AccountQueryServiceClient
	if customer == nil || account == nil {
		t.Log("compile-time contract check")
	}
}
```

- [ ] **Step 2: Run the test and verify missing packages**

Run:

```bash
cd templates/bank
go test ./internal/platform/grpcx
```

Expected: compile failure because generated packages do not exist.

- [ ] **Step 3: Define exact Protobuf services**

Customer response fields: `customer_id`, `name`, `customer_type`,
`kyc_status`, `status`, and repeated `risk_tags`.

Account response fields: `account_no`, `customer_id`, `currency`, `status`,
`ledger_balance_minor`, and `available_balance_minor`.

Both requests contain the identifying string and `request_id`.

- [ ] **Step 4: Configure deterministic generation**

Use:

```yaml
version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.35.1
    out: gen
    opt: paths=source_relative
  - remote: buf.build/grpc/go:v1.5.1
    out: gen
    opt: paths=source_relative
```

Add:

```make
.PHONY: proto
proto:
	docker run --rm -v "$(CURDIR):/workspace" -w /workspace bufbuild/buf:1.47.2 generate
```

- [ ] **Step 5: Generate, add dependencies, and run tests**

Run:

```bash
cd templates/bank
make proto
go get google.golang.org/grpc@v1.67.1
go get google.golang.org/protobuf@v1.35.1
go mod tidy
go test ./internal/platform/grpcx
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/bank/buf.yaml templates/bank/buf.gen.yaml templates/bank/proto templates/bank/gen templates/bank/go.mod templates/bank/go.sum templates/bank/Makefile templates/bank/internal/platform/grpcx/contracts_test.go
git commit -m "feat(bank): define internal gRPC query contracts"
```

### Task 2: Implement gRPC Runtime and Query Servers

**Files:**
- Create: `templates/bank/internal/platform/grpcx/client.go`
- Create: `templates/bank/internal/platform/grpcx/client_test.go`
- Create: `templates/bank/internal/platform/grpcx/server.go`
- Create: `templates/bank/internal/platform/grpcx/server_test.go`
- Create: `templates/bank/internal/customer/api/grpc.go`
- Create: `templates/bank/internal/customer/api/grpc_test.go`
- Create: `templates/bank/internal/corebanking/api/grpc.go`
- Create: `templates/bank/internal/corebanking/api/grpc_test.go`
- Modify: `templates/bank/cmd/customer/main.go`
- Modify: `templates/bank/cmd/core-banking/main.go`

**Interfaces:**
- Produces: `grpcx.Dial(context.Context, grpcx.ClientConfig) (*grpc.ClientConn, error)`.
- Produces: `grpcx.NewServer(grpcx.ServerConfig) *grpc.Server`.
- Produces: customer and account query server implementations.

- [ ] **Step 1: Write failing runtime tests**

Test that the default service config is:

```json
{"loadBalancingConfig":[{"round_robin":{}}],"healthCheckConfig":{"serviceName":""}}
```

Test that `NewServer` registers `grpc.health.v1.Health` and changes from
`NOT_SERVING` to `SERVING` only after the dependency readiness callback passes.

- [ ] **Step 2: Run focused tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/grpcx
```

Expected: compile failure for missing runtime functions.

- [ ] **Step 3: Implement client and server**

Use:

```go
type ClientConfig struct {
	Target  string
	Timeout time.Duration
}

type ServerConfig struct {
	Ready func(context.Context) error
}

func Dial(ctx context.Context, cfg ClientConfig) (*grpc.ClientConn, error)
func NewServer(cfg ServerConfig) *grpc.Server
```

`Dial` uses `grpc.NewClient` with insecure transport credentials for the local
template and `grpc.WithDefaultServiceConfig` containing `round_robin`.

- [ ] **Step 4: Implement query adapters**

Customer maps its repository result to `CustomerSnapshot`. Core-banking maps
its account and balance repositories to `AccountSnapshot`; it computes
`available_balance_minor` as the ledger balance until funds holds are added by
the payment Saga plan.

- [ ] **Step 5: Run tests**

Run:

```bash
cd templates/bank
go test ./internal/platform/grpcx ./internal/customer/api ./internal/corebanking/api
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/bank/internal/platform/grpcx templates/bank/internal/customer/api templates/bank/internal/corebanking/api templates/bank/cmd/customer/main.go templates/bank/cmd/core-banking/main.go
git commit -m "feat(bank): serve internal customer and account gRPC"
```

### Task 3: Replace Bank Service-to-Service HTTP with gRPC

**Files:**
- Create: `templates/bank/internal/platform/serviceclient/customer.go`
- Create: `templates/bank/internal/platform/serviceclient/account.go`
- Modify: `templates/bank/internal/platform/serviceclient/client.go`
- Modify: `templates/bank/internal/platform/serviceclient/client_test.go`
- Modify: repositories/services that construct cross-service clients under `templates/bank/internal/*`
- Modify: `templates/bank/cmd/payment/main.go`
- Modify: `templates/bank/cmd/reward/main.go`
- Modify: `templates/bank/cmd/risk/main.go`
- Modify: `templates/bank/cmd/loan/main.go`
- Modify: `templates/bank/cmd/wealth/main.go`

**Interfaces:**
- Consumes: generated gRPC clients from Task 1 and `grpcx.Dial` from Task 2.
- Produces: domain-facing `GetCustomer` and `GetAccount` adapters with no HTTP dependency.

- [ ] **Step 1: Change service-client tests to use bufconn gRPC**

Use:

```go
type CustomerReader interface {
	GetCustomer(context.Context, string, string) (Customer, error)
}

type AccountReader interface {
	GetAccount(context.Context, string, string) (Account, error)
}
```

Assert gRPC `NotFound` maps to the existing domain not-found error and
`Unavailable` remains retryable.

- [ ] **Step 2: Run tests and verify the old HTTP client fails the contract**

Run:

```bash
cd templates/bank
go test ./internal/platform/serviceclient
```

Expected: compile failure until the adapters are implemented.

- [ ] **Step 3: Implement gRPC adapters and update composition**

Use environment targets:

```text
CUSTOMER_GRPC_TARGET=dns:///customer:9090
CORE_BANKING_GRPC_TARGET=dns:///core-banking:9090
```

Remove `CORE_BANKING_URL` and `CUSTOMER_URL` from every bank process. Retain no
`net/http` client in `internal/platform/serviceclient`.

- [ ] **Step 4: Prove service-to-service HTTP is gone**

Run:

```bash
cd templates/bank
go test ./...
! rg -n 'CUSTOMER_URL|CORE_BANKING_URL|http://customer|http://core-banking' . --glob '!README.md' --glob '!ARCHITECTURE.md'
```

Expected: PASS and no matches.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal templates/bank/cmd
git commit -m "refactor(bank): replace internal HTTP with gRPC"
```

### Task 4: Add Bank HTTP Lifecycle and Messaging Foundation

**Files:**
- Create: `templates/bank/internal/platform/httpx/server.go`
- Create: `templates/bank/internal/platform/httpx/server_test.go`
- Create: `templates/bank/internal/platform/messaging/envelope.go`
- Create: `templates/bank/internal/platform/messaging/envelope_test.go`
- Create: `templates/bank/internal/platform/messaging/rabbitmq.go`
- Create: `templates/bank/internal/platform/messaging/rabbitmq_test.go`
- Create: `templates/bank/db/migrations/shared.sql`
- Modify: `templates/bank/cmd/core-banking/main.go`
- Modify: `templates/bank/cmd/customer/main.go`
- Modify: `templates/bank/cmd/payment/main.go`
- Modify: `templates/bank/cmd/reward/main.go`
- Modify: `templates/bank/cmd/risk/main.go`
- Modify: `templates/bank/cmd/loan/main.go`
- Modify: `templates/bank/cmd/wealth/main.go`
- Modify: `templates/bank/go.mod`
- Modify: `templates/bank/go.sum`

**Interfaces:**
- Produces: `/livez`, `/readyz`, `/metrics`, graceful REST shutdown.
- Produces: schema-versioned `messaging.Envelope`, confirmed publisher, manual-ack consumer primitives.

- [ ] **Step 1: Write lifecycle and envelope tests**

Assert `/livez` remains 200 while `/readyz` returns 503 during drain. Assert
the envelope round-trips:

```go
type Envelope struct {
	MessageID      string          `json:"message_id"`
	MessageType    string          `json:"message_type"`
	SchemaVersion  int             `json:"schema_version"`
	WorkflowID     string          `json:"workflow_id,omitempty"`
	ActionName     string          `json:"action_name,omitempty"`
	CommandID      string          `json:"command_id,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	CorrelationID  string          `json:"correlation_id"`
	CausationID    string          `json:"causation_id,omitempty"`
	OccurredAt     time.Time       `json:"occurred_at"`
	Payload        json.RawMessage `json:"payload"`
}
```

- [ ] **Step 2: Run tests and verify failure**

Run:

```bash
cd templates/bank
go test ./internal/platform/httpx ./internal/platform/messaging
```

Expected: compile failure.

- [ ] **Step 3: Implement the minimum platform**

Expose these exact lifecycle interfaces:

```go
type ServerConfig struct {
	Service, Instance, Addr string
	Handler http.Handler
	Ready func(context.Context) error
	Registry *prometheus.Registry
	ShutdownTimeout time.Duration
}
func NewServer(ServerConfig) *Server
func (s *Server) ListenAndServe() error
func (s *Server) Shutdown(context.Context) error
```

Expose these exact messaging interfaces:

```go
func NewEnvelope(messageType, correlationID string, payload json.RawMessage, now func() time.Time) Envelope
func NewRabbitPublisher(*amqp.Channel, string) (*RabbitPublisher, error)
func (p *RabbitPublisher) Publish(context.Context, string, Envelope) error
func ProcessDelivery(context.Context, *sql.Tx, string, amqp.Delivery, func(context.Context, Envelope) error, RetryPolicy) error
```

Publish persistent mandatory messages, wait for Publisher Confirm, insert Inbox
before handler execution, and acknowledge only after the transaction commits.
Invalid envelopes reject to DLQ; transient errors route through the bounded
retry queue.

Add:

```bash
cd templates/bank
go get github.com/rabbitmq/amqp091-go@v1.10.0
go get github.com/prometheus/client_golang@v1.21.0
go mod tidy
```

Update all seven entrypoints to use `HTTP_ADDR=:8080`, the new `httpx.Server`,
dependency-backed readiness, and one shared signal context. Customer and
core-banking also serve gRPC on `GRPC_ADDR=:9090`; the other five entrypoints do
not open a gRPC listener.

- [ ] **Step 4: Add shared Outbox/Inbox schema**

`shared.sql` defines `outbox_message` and `inbox_message` with unique message
IDs, routing key, attempts, `claim_token`, `claimed_at`, dispatched timestamp,
and last error.

- [ ] **Step 5: Run tests and commit**

Run:

```bash
cd templates/bank
go test ./internal/platform/httpx ./internal/platform/messaging
go test ./...
```

Then:

```bash
git add templates/bank/internal/platform templates/bank/db/migrations/shared.sql templates/bank/go.mod templates/bank/go.sum
git commit -m "feat(bank): add service lifecycle and messaging chassis"
```

### Task 5: Replace the Local Topology with Dedicated State and Traefik

**Files:**
- Create: `templates/bank/compose.yaml`
- Delete: `templates/bank/docker-compose.yaml`
- Create: `templates/bank/deploy/traefik/traefik.yaml`
- Create: `templates/bank/deploy/rabbitmq/definitions.json`
- Modify: `templates/bank/Dockerfile`
- Modify: `templates/bank/Makefile`
- Modify: `templates/bank/template.yaml`

**Interfaces:**
- Produces: `make up`, `make scale SERVICE=payment REPLICAS=3`, `make down`, and gateway `http://localhost:18000`.

- [ ] **Step 1: Add failing static topology tests**

Create a shell test that asserts:

```bash
docker compose -f compose.yaml config --quiet
test "$(docker compose -f compose.yaml config --services | grep -c -- '-db$')" -eq 7
test "$(docker compose -f compose.yaml config --format json |
  jq '[.services | to_entries[] | select(.value.ports != null) | .key] == [\"traefik\"]')" = "true"
rg -n 'replicas: 2' compose.yaml
```

- [ ] **Step 2: Build the Compose topology**

Define seven PostgreSQL services and volumes, RabbitMQ, Traefik, migrations,
seed, and seven application services. Use common application defaults:

```yaml
read_only: true
tmpfs: [/tmp]
cap_drop: [ALL]
security_opt: [no-new-privileges:true]
stop_grace_period: 30s
mem_reservation: 128m
mem_limit: 512m
cpus: 1.0
```

Only Traefik publishes `18000:8080`. Public route labels contain only
`/api/v1/...`.

- [ ] **Step 3: Define broker topology**

Create exchanges `bank.commands`, `bank.events`, `bank.retry`, and `bank.dlx`.
Create durable queues `risk.commands`, `core-banking.commands`,
`payment.workflow.events`, and `reward.payment-events`, each command queue
having `.retry` and `.dlq` companions.

- [ ] **Step 4: Add scaling and config validation targets**

Use:

```make
scale:
	@test -n "$(SERVICE)" -a -n "$(REPLICAS)"
	docker compose -f compose.yaml up -d --wait --no-deps --scale $(SERVICE)=$(REPLICAS) $(SERVICE)

config-check:
	docker compose -f compose.yaml config --quiet
```

- [ ] **Step 5: Run static validation and unit tests**

Run:

```bash
cd templates/bank
make config-check
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/bank/compose.yaml templates/bank/deploy templates/bank/Dockerfile templates/bank/Makefile templates/bank/template.yaml
git rm templates/bank/docker-compose.yaml
git commit -m "feat(bank): add scalable gateway and dedicated state topology"
```

### Task 6: Add Kubernetes Application Foundations

**Files:**
- Create: `templates/bank/deploy/k8s/base/kustomization.yaml`
- Create: `templates/bank/deploy/k8s/base/namespace.yaml`
- Create: `templates/bank/deploy/k8s/base/config.yaml`
- Create: `templates/bank/deploy/k8s/base/apps.yaml`
- Create: `templates/bank/deploy/k8s/base/services.yaml`
- Create: `templates/bank/deploy/k8s/base/gateway.yaml`
- Create: `templates/bank/deploy/k8s/base/availability.yaml`
- Test: `templates/bank/deploy/k8s/test.sh`

**Interfaces:**
- Produces: a renderable application-only base; runnable state overlays are delivered by the operational-closure plan.

- [ ] **Step 1: Write a failing manifest assertion script**

Assert exact counts:

```bash
rendered=$(mktemp)
kubectl kustomize deploy/k8s/base >"${rendered}"
test "$(yq 'select(.kind == "Deployment") | .metadata.name' "${rendered}" | wc -l)" -eq 7
test "$(yq 'select(.kind == "HorizontalPodAutoscaler") | .metadata.name' "${rendered}" | wc -l)" -eq 7
test "$(yq 'select(.kind == "PodDisruptionBudget") | .metadata.name' "${rendered}" | wc -l)" -eq 3
! rg -n '/internal/|9090.*Ingress' "${rendered}"
```

- [ ] **Step 2: Create Deployments and two Service shapes**

Each application gets:

```yaml
ports:
  - {name: http, containerPort: 8080}
  - {name: grpc, containerPort: 9090}
```

Each gets a REST ClusterIP Service and a `<name>-grpc` headless Service with
`clusterIP: None` and `publishNotReadyAddresses: false`.

- [ ] **Step 3: Add availability and gateway resources**

Replica defaults match the table in Global Constraints. Add PDBs for
core-banking, payment, and risk. Add CPU HPAs for all seven services and
document the Metrics Server dependency in `availability.yaml`. Ingress contains
only public REST prefixes and targets only REST Services.

- [ ] **Step 4: Render and verify**

Run:

```bash
cd templates/bank
kubectl kustomize deploy/k8s/base >/tmp/bank-k8s.yaml
bash deploy/k8s/test.sh
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add templates/bank/deploy/k8s
git commit -m "feat(bank): add Kubernetes application foundations"
```

### Task 7: Verify and Package the Chassis Checkpoint

**Files:**
- Modify: `templates/bank/README.md`
- Modify: `templates/bank/ARCHITECTURE.md`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `internal/template/templates.tar`

**Interfaces:**
- Produces: a generated bank project with the new topology and internal gRPC.

- [ ] **Step 1: Update documentation**

Document gateway port 18000, replica defaults, gRPC/MQ boundaries, dedicated
databases, local memory budget, scaling, Kubernetes base limitations, and
stateful HA non-goals.

- [ ] **Step 2: Run checkpoint verification**

Run:

```bash
cd templates/bank
go build ./...
go test ./...
make config-check
kubectl kustomize deploy/k8s/base >/tmp/bank-k8s.yaml
cd ../..
go generate ./internal/template
go test ./...
git diff --check
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add templates/bank/README.md templates/bank/ARCHITECTURE.md README.md README.zh-CN.md internal/template/templates.tar
git commit -m "build(bank): package production chassis"
```
