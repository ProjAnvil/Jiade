# Commerce Hard-Gap Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove public access to commerce internal routes, make OpenTelemetry traces reach Jaeger, and add complete commerce CI coverage.

**Architecture:** Traefik and Kubernetes Ingress expose only public REST prefixes; service-to-service HTTP continues over Docker/Kubernetes service DNS. A shared telemetry provider instruments HTTP and RabbitMQ with W3C context propagation and exports OTLP spans through the collector to Jaeger.

**Tech Stack:** Go 1.22, OpenTelemetry Go 1.32.0, OTel HTTP instrumentation 0.57.0, RabbitMQ 4, Traefik 3, OTel Collector 0.108.0, Jaeger 1.60, Docker Compose, Kubernetes Kustomize, GitHub Actions.

## Global Constraints

- Preserve the current external commerce REST paths and response contracts.
- `/internal/v1/...` must remain reachable over service DNS but never through Traefik or Ingress.
- Keep at-least-once delivery, transactional Outbox/Inbox, manual acknowledgement, retry queues, and DLQ behavior unchanged.
- Generated templates remain self-contained; commerce cannot import bank.
- OTel failure must not prevent a business service from starting.
- Every task ends with focused tests and a commit.

---

## File Map

- `templates/commerce/compose.yaml`: public Traefik route allowlist and OTel environment.
- `templates/commerce/deploy/k8s/gateway.yaml`: public Ingress allowlist.
- `templates/commerce/test/smoke.sh`: negative gateway probes.
- `templates/commerce/internal/platform/telemetry/provider.go`: OTel provider lifecycle.
- `templates/commerce/internal/platform/telemetry/propagation.go`: AMQP trace carrier.
- `templates/commerce/internal/platform/telemetry/log.go`: trace-aware structured logging.
- `templates/commerce/internal/platform/httpx/server.go`: inbound HTTP spans.
- `templates/commerce/internal/platform/client/client.go`: outbound HTTP spans.
- `templates/commerce/internal/platform/messaging/rabbitmq.go`: message publish/consume spans.
- `templates/commerce/deploy/otel/collector.yaml`: Jaeger exporter.
- `templates/commerce/deploy/grafana/provisioning/dashboards/commerce.yaml`: dashboard provider.
- `templates/commerce/deploy/grafana/dashboards/commerce-overview.json`: minimal dashboard.
- `templates/commerce/test/trace-smoke.sh`: Jaeger trace verification.
- `.github/workflows/ci.yml`: commerce unit/static and E2E jobs.

### Task 1: Close Public Internal Routes

**Files:**
- Modify: `templates/commerce/compose.yaml`
- Modify: `templates/commerce/deploy/k8s/gateway.yaml`
- Modify: `templates/commerce/test/smoke.sh`
- Modify: `templates/commerce/README.md`
- Test: `templates/commerce/internal/platform/httpx/server_test.go`

**Interfaces:**
- Consumes: existing `/internal/v1/catalog`, `/internal/v1/customer`, and `/internal/v1/reservations` handlers over service DNS.
- Produces: Gateway behavior where every `/internal/v1/*` request returns HTTP 404.

- [ ] **Step 1: Add the failing route-isolation smoke gate**

Append this exact function and invocation to `templates/commerce/test/smoke.sh`:

```bash
gate_internal_routes_are_private() {
  local path code
  for path in \
    /internal/v1/catalog/products/does-not-matter \
    /internal/v1/customer/customers/does-not-matter \
    /internal/v1/reservations/does-not-matter
  do
    code=$(curl -sS -o /dev/null -w '%{http_code}' "${GATEWAY}${path}")
    if [[ "${code}" != "404" ]]; then
      fail "gateway exposed ${path}: status=${code}, want 404"
    fi
  done
}

gate_internal_routes_are_private
```

- [ ] **Step 2: Run the static probe and demonstrate the current exposure**

Run:

```bash
cd templates/commerce
rg -n 'traefik\.http\.routers\..*internal/v1|path: /internal/v1' compose.yaml deploy/k8s/gateway.yaml
```

Expected: matches in both files, proving the new gate would fail against a running stack.

- [ ] **Step 3: Restrict Compose and Ingress to public prefixes**

Change the three Traefik rules to:

```yaml
- traefik.http.routers.catalog.rule=PathPrefix(`/api/v1/products`)
- traefik.http.routers.customer.rule=PathPrefix(`/api/v1/customers`)
- traefik.http.routers.inventory.rule=PathPrefix(`/api/v1/inventory`) || PathPrefix(`/api/v1/reservations`)
```

Delete the `/internal/v1/catalog`, `/internal/v1/customer`, and
`/internal/v1/reservations` path blocks from `deploy/k8s/gateway.yaml`.

- [ ] **Step 4: Document and statically verify the boundary**

Replace the README statement that internal routes pass through Traefik with:

```markdown
Internal routes (`/internal/v1/...`) are reachable only over the service
network by using service DNS. Traefik and the Kubernetes Ingress intentionally
have no rules for these paths.
```

Run:

```bash
cd templates/commerce
docker compose -f compose.yaml config --quiet
! rg -n 'traefik\.http\.routers\..*internal/v1|path: /internal/v1' compose.yaml deploy/k8s/gateway.yaml
bash -n test/smoke.sh
```

Expected: all commands exit 0.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/compose.yaml templates/commerce/deploy/k8s/gateway.yaml templates/commerce/test/smoke.sh templates/commerce/README.md
git commit -m "fix(commerce): keep internal routes off the gateway"
```

### Task 2: Add a Real OpenTelemetry Provider

**Files:**
- Modify: `templates/commerce/go.mod`
- Modify: `templates/commerce/go.sum`
- Create: `templates/commerce/internal/platform/telemetry/provider.go`
- Create: `templates/commerce/internal/platform/telemetry/provider_test.go`
- Create: `templates/commerce/internal/platform/telemetry/propagation.go`
- Create: `templates/commerce/internal/platform/telemetry/propagation_test.go`
- Modify: `templates/commerce/internal/platform/telemetry/log.go`

**Interfaces:**
- Produces: `telemetry.New(context.Context, telemetry.Config) (*telemetry.Provider, error)`.
- Produces: `(*Provider).Shutdown(context.Context) error`.
- Produces: `telemetry.InjectAMQP(context.Context, amqp.Table)` and `telemetry.ExtractAMQP(context.Context, amqp.Table) context.Context`.
- Produces: `telemetry.TraceFields(context.Context) []any`.

- [ ] **Step 1: Write failing provider and propagation tests**

Create tests asserting disabled startup, resource identity, and AMQP round-trip:

```go
func TestAMQPPropagationRoundTrip(t *testing.T) {
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	ctx, span := provider.Tracer("test").Start(context.Background(), "publish")
	defer span.End()

	headers := amqp.Table{}
	InjectAMQP(ctx, headers)
	extracted := ExtractAMQP(context.Background(), headers)
	if got, want := trace.SpanContextFromContext(extracted).TraceID(), span.SpanContext().TraceID(); got != want {
		t.Fatalf("trace ID=%s, want %s", got, want)
	}
}

func TestNewDisabledProviderDoesNotExport(t *testing.T) {
	provider, err := New(context.Background(), Config{Service: "catalog", Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run the focused tests to verify failure**

Run:

```bash
cd templates/commerce
go test ./internal/platform/telemetry
```

Expected: compile failure because `Config`, `Provider`, `New`, `InjectAMQP`, and `ExtractAMQP` do not exist.

- [ ] **Step 3: Add pinned OTel dependencies**

Run:

```bash
cd templates/commerce
go get go.opentelemetry.io/otel@v1.32.0
go get go.opentelemetry.io/otel/sdk@v1.32.0
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc@v1.32.0
go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.57.0
go mod tidy
```

- [ ] **Step 4: Implement the provider and AMQP carrier**

Use this public shape:

```go
type Config struct {
	Service  string
	Instance string
	Endpoint string
	Enabled  bool
	Insecure bool
}

type Provider struct {
	tracer *sdktrace.TracerProvider
}

func New(ctx context.Context, cfg Config) (*Provider, error)
func Disabled() *Provider
func (p *Provider) Shutdown(ctx context.Context) error
func InjectAMQP(ctx context.Context, headers amqp.Table)
func ExtractAMQP(ctx context.Context, headers amqp.Table) context.Context
func TraceFields(ctx context.Context) []any
```

`New` must install `propagation.NewCompositeTextMapPropagator(
propagation.TraceContext{}, propagation.Baggage{})`, configure
`resource.WithAttributes(semconv.ServiceName(cfg.Service),
semconv.ServiceInstanceID(cfg.Instance))`, and use a batch span processor when
enabled. `TraceFields` returns `trace_id` and `span_id` only for a valid span.

- [ ] **Step 5: Run telemetry tests**

Run:

```bash
cd templates/commerce
go test ./internal/platform/telemetry
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/commerce/go.mod templates/commerce/go.sum templates/commerce/internal/platform/telemetry
git commit -m "feat(commerce): add OpenTelemetry provider"
```

### Task 3: Instrument HTTP and RabbitMQ

**Files:**
- Modify: `templates/commerce/internal/platform/httpx/server.go`
- Modify: `templates/commerce/internal/platform/httpx/server_test.go`
- Modify: `templates/commerce/internal/platform/httpx/middleware.go`
- Modify: `templates/commerce/internal/platform/client/client.go`
- Modify: `templates/commerce/internal/platform/client/client_test.go`
- Modify: `templates/commerce/internal/platform/messaging/rabbitmq.go`
- Modify: `templates/commerce/internal/platform/messaging/rabbitmq_test.go`
- Modify: `templates/commerce/internal/inventory/consumer.go`
- Modify: `templates/commerce/internal/order/consumer.go`
- Modify: `templates/commerce/internal/payment/consumer.go`
- Modify: `templates/commerce/internal/fulfillment/consumer.go`

**Interfaces:**
- Consumes: telemetry provider and AMQP propagation from Task 2.
- Produces: inbound `commerce.http.server`, outbound `commerce.http.client`, `messaging.publish`, and `messaging.consume` spans.
- Produces: `ProcessRabbitDelivery` handlers receive the extracted
  `context.Context`: `func(context.Context, Event) error`.

- [ ] **Step 1: Write failing instrumentation tests**

Use an in-memory span recorder and assert:

```go
func TestServerCreatesHTTPSpan(t *testing.T) {
	recorder, provider := newSpanRecorder(t)
	otel.SetTracerProvider(provider)
	server := NewServer(ServerConfig{Service: "catalog", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	server.Handler().ServeHTTP(httptest.NewRecorder(), request)
	if got := spanNames(recorder.Ended()); !slices.Contains(got, "GET /api/v1/products") {
		t.Fatalf("spans=%v", got)
	}
}
```

In the Rabbit publisher fake, inspect `amqp.Publishing.Headers` and assert a
valid `traceparent`. In the delivery handler, assert the handler context uses
the trace ID from delivery headers.

- [ ] **Step 2: Run focused tests and verify failure**

Run:

```bash
cd templates/commerce
go test ./internal/platform/httpx ./internal/platform/client ./internal/platform/messaging
```

Expected: new span assertions fail.

- [ ] **Step 3: Add HTTP instrumentation**

Wrap the final server handler:

```go
server.handler = otelhttp.NewHandler(
	requestID(serviceInstance(config.Instance,
		accessLog(config.Logger, config.Service,
			limitBody(config.RequestBodyLimit, recoverPanic(config.Logger, config.Service, mux))))),
	config.Service+".http",
)
```

Wrap the resilient client's base transport once:

```go
transport := config.Transport
if transport == nil {
	transport = http.DefaultTransport
}
client.transport = otelhttp.NewTransport(transport)
```

After choosing the request ID, attach it to the active server span:

```go
trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("request.id", id))
```

Append `telemetry.TraceFields(r.Context())...` to access and panic log
attributes.

- [ ] **Step 4: Add RabbitMQ context propagation**

Before publishing:

```go
ctx, span := otel.Tracer("commerce/messaging").Start(ctx, "messaging.publish",
	trace.WithAttributes(attribute.String("messaging.destination.name", exchange)))
defer span.End()
telemetry.InjectAMQP(ctx, message.Headers)
```

Before processing:

```go
ctx = telemetry.ExtractAMQP(ctx, delivery.Headers)
ctx, span := otel.Tracer("commerce/messaging").Start(ctx, "messaging.consume",
	trace.WithSpanKind(trace.SpanKindConsumer))
defer span.End()
```

Change `ProcessRabbitDelivery` and `ProcessRabbitDeliveryForRetryQueue` from
`handler func(Event) error` to:

```go
handler func(context.Context, Event) error
```

Update inventory, order, payment, and fulfillment consumers to pass the
extracted handler context into their store/service calls.

Record publish/handler errors with `span.RecordError` and set error status.

- [ ] **Step 5: Run focused and full commerce tests**

Run:

```bash
cd templates/commerce
go test ./internal/platform/httpx ./internal/platform/client ./internal/platform/messaging
go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/commerce/internal/platform/httpx templates/commerce/internal/platform/client templates/commerce/internal/platform/messaging templates/commerce/internal/inventory/consumer.go templates/commerce/internal/order/consumer.go templates/commerce/internal/payment/consumer.go templates/commerce/internal/fulfillment/consumer.go
git commit -m "feat(commerce): trace HTTP and RabbitMQ"
```

### Task 4: Wire Every Commerce Process and Jaeger Export

**Files:**
- Modify: `templates/commerce/internal/platform/config/config.go`
- Modify: `templates/commerce/internal/platform/config/config_test.go`
- Modify: `templates/commerce/cmd/catalog/main.go`
- Modify: `templates/commerce/cmd/customer/main.go`
- Modify: `templates/commerce/cmd/inventory/main.go`
- Modify: `templates/commerce/cmd/order/main.go`
- Modify: `templates/commerce/cmd/payment/main.go`
- Modify: `templates/commerce/cmd/fulfillment/main.go`
- Modify: `templates/commerce/compose.yaml`
- Modify: `templates/commerce/compose.observability.yaml`
- Modify: `templates/commerce/deploy/otel/collector.yaml`

**Interfaces:**
- Consumes: `telemetry.New` from Task 2.
- Produces: validated `OTEL_ENABLED`, `OTEL_EXPORTER_OTLP_ENDPOINT`, and `OTEL_EXPORTER_OTLP_INSECURE` settings.

- [ ] **Step 1: Add failing configuration tests**

Add assertions:

```go
func TestLoadTelemetryDefaults(t *testing.T) {
	settings := loadWithRequiredEnvironment(t)
	if settings.Telemetry.Enabled {
		t.Fatal("telemetry enabled by default")
	}
	if settings.Telemetry.Endpoint != "otel-collector:4317" {
		t.Fatalf("endpoint=%q", settings.Telemetry.Endpoint)
	}
}
```

- [ ] **Step 2: Run configuration tests**

Run:

```bash
cd templates/commerce
go test ./internal/platform/config
```

Expected: compile failure because `Settings.Telemetry` does not exist.

- [ ] **Step 3: Implement settings and process lifecycle**

Add:

```go
type Telemetry struct {
	Enabled  bool
	Endpoint string
	Insecure bool
}
```

In each service `run`, initialize after config validation:

```go
observability, err := telemetry.New(processContext, telemetry.Config{
	Service: settings.Service, Instance: settings.InstanceID,
	Enabled: settings.Telemetry.Enabled, Endpoint: settings.Telemetry.Endpoint,
	Insecure: settings.Telemetry.Insecure,
})
if err != nil {
	logger.Warn("telemetry disabled after initialization failure", "error", err)
	observability = telemetry.Disabled()
}
defer func() {
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = observability.Shutdown(shutdownContext)
}()
```

- [ ] **Step 4: Configure Compose and Collector**

Add service environment:

```yaml
OTEL_ENABLED: ${OTEL_ENABLED:-false}
OTEL_EXPORTER_OTLP_ENDPOINT: otel-collector:4317
OTEL_EXPORTER_OTLP_INSECURE: "true"
```

The observability overlay sets `OTEL_ENABLED: "true"` for all six services.
Declare an environment override for each service:

```yaml
catalog:
  environment: &otel-service-env
    OTEL_ENABLED: "true"
customer:
  environment: *otel-service-env
inventory:
  environment: *otel-service-env
order:
  environment: *otel-service-env
payment:
  environment: *otel-service-env
fulfillment:
  environment: *otel-service-env
```

Add this collector exporter and change the traces pipeline:

```yaml
exporters:
  otlp/jaeger:
    endpoint: jaeger:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [memory_limiter, batch]
      exporters: [otlp/jaeger]
```

- [ ] **Step 5: Verify configuration and tests**

Run:

```bash
cd templates/commerce
go test ./...
docker compose -f compose.yaml -f compose.observability.yaml config --quiet
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add templates/commerce/internal/platform/config templates/commerce/cmd templates/commerce/compose.yaml templates/commerce/compose.observability.yaml templates/commerce/deploy/otel/collector.yaml
git commit -m "feat(commerce): export service traces to Jaeger"
```

### Task 5: Provision Dashboard and Trace Smoke Gate

**Files:**
- Create: `templates/commerce/deploy/grafana/provisioning/dashboards/commerce.yaml`
- Create: `templates/commerce/deploy/grafana/dashboards/commerce-overview.json`
- Modify: `templates/commerce/compose.observability.yaml`
- Create: `templates/commerce/test/trace-smoke.sh`
- Modify: `templates/commerce/Makefile`

**Interfaces:**
- Produces: `make trace-smoke`, which fails unless Jaeger contains a commerce trace.

- [ ] **Step 1: Add the failing trace smoke script**

Create an executable script that:

```bash
#!/usr/bin/env bash
set -euo pipefail
gateway=${GATEWAY:-http://localhost:18100}
jaeger_network=${JAEGER_NETWORK:-commerce-obs}
request_id="trace-smoke-$(date +%s)"

curl -fsS -H "X-Request-ID: ${request_id}" "${gateway}/api/v1/products?limit=1" >/dev/null
for _ in $(seq 1 30); do
  if docker run --rm --network "${jaeger_network}" curlimages/curl:8.10.1 \
    -fsS "http://jaeger:16686/api/traces?service=catalog&limit=20" |
    jq -e --arg request_id "${request_id}" '.. | strings | select(. == $request_id)' >/dev/null
  then
    exit 0
  fi
  sleep 1
done
echo "catalog trace for ${request_id} did not reach Jaeger" >&2
exit 1
```

- [ ] **Step 2: Add dashboard provisioning**

Mount:

```yaml
- ./deploy/grafana/provisioning:/etc/grafana/provisioning:ro
- ./deploy/grafana/dashboards:/var/lib/grafana/dashboards:ro
```

Provision `/var/lib/grafana/dashboards` and add a dashboard containing exact
panels for `rate(http_requests_total[5m])`,
`histogram_quantile(0.95, sum by (le,service)
(rate(http_request_duration_seconds_bucket[5m])))`, and
`max by (service) (outbox_oldest_age_seconds)`.

- [ ] **Step 3: Add Make targets and validate files**

Add:

```make
.PHONY: trace-smoke
trace-smoke:
	GATEWAY=$(GATEWAY) bash test/trace-smoke.sh
```

Run:

```bash
cd templates/commerce
bash -n test/trace-smoke.sh
jq empty deploy/grafana/dashboards/commerce-overview.json
docker compose -f compose.yaml -f compose.observability.yaml config --quiet
```

Expected: PASS.

- [ ] **Step 4: Run trace E2E**

Run:

```bash
cd templates/commerce
make observability
make trace-smoke
```

Expected: `trace-smoke.sh` exits 0.

- [ ] **Step 5: Commit**

```bash
git add templates/commerce/deploy/grafana templates/commerce/compose.observability.yaml templates/commerce/test/trace-smoke.sh templates/commerce/Makefile
git commit -m "feat(commerce): provision observability verification"
```

### Task 6: Add Commerce CI Gates

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `Makefile`
- Modify: `templates/commerce/README.md`

**Interfaces:**
- Produces: `commerce` and `commerce-e2e` GitHub Actions jobs.

- [ ] **Step 1: Add a local CI aggregate**

Add:

```make
.PHONY: commerce-ci
commerce-ci:
	cd templates/commerce && go build ./...
	cd templates/commerce && go test ./...
	cd templates/commerce && go test -race ./internal/platform/...
	cd templates/commerce && $(MAKE) config-check
	kubectl kustomize templates/commerce/deploy/k8s >/tmp/commerce-k8s.yaml
```

- [ ] **Step 2: Add the unit/static CI job**

Add a `commerce` job using Go 1.22 and:

```yaml
- uses: azure/setup-kubectl@v4
- run: cd templates/commerce && go build ./...
- run: cd templates/commerce && go test ./...
- run: cd templates/commerce && go test -race ./internal/platform/...
- run: cd templates/commerce && make config-check
- run: kubectl kustomize templates/commerce/deploy/k8s >/tmp/commerce-k8s.yaml
```

- [ ] **Step 3: Add the Commerce E2E job**

Use:

```yaml
commerce-e2e:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: '1.22'
    - name: Start commerce
      run: |
        cd templates/commerce
        make up CUSTOMER_REPLICAS=1 INVENTORY_REPLICAS=1 ORDER_REPLICAS=1 PAYMENT_REPLICAS=1 FULFILLMENT_REPLICAS=1
    - name: Smoke
      run: cd templates/commerce && make smoke
    - name: Observability trace
      run: cd templates/commerce && make observability && make trace-smoke
    - name: Capture logs
      if: failure()
      run: cd templates/commerce && docker compose -f compose.yaml -f compose.observability.yaml logs --no-color
    - name: Cleanup
      if: always()
      run: cd templates/commerce && docker compose -f compose.yaml -f compose.observability.yaml down --volumes --remove-orphans
```

- [ ] **Step 4: Run the local static equivalent**

Run:

```bash
make commerce-ci
```

Expected: PASS.

- [ ] **Step 5: Update documentation and commit**

Document the internal-route boundary, trace smoke command, and CI gates, then:

```bash
git add .github/workflows/ci.yml Makefile templates/commerce/README.md
git commit -m "ci: verify commerce topology and traces"
```

### Task 7: Repackage and Verify the Commerce Checkpoint

**Files:**
- Modify: `internal/template/templates.tar`
- Test: `internal/template/render_test.go`

**Interfaces:**
- Produces: a generated `commerce` project containing all Task 1-6 changes.

- [ ] **Step 1: Regenerate the embedded templates**

Run:

```bash
go generate ./internal/template
```

- [ ] **Step 2: Run the checkpoint verification**

Run:

```bash
go test ./...
cd templates/commerce && go test ./...
cd ../.. && git diff --check
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/template/templates.tar
git commit -m "build(commerce): package route and telemetry fixes"
```
