# DCN 模板生产化增强 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 `templates/dcn` 补齐生产形态：独立批量调度服务 + 单元结息、Traefik 统一接入层、真实规模 seed、Prometheus/Grafana/console 观测体系。

**Architecture:** 依据 `docs/superpowers/specs/2026-08-02-dcn-production-realism-design.md`。新增 batch-scheduler（独立库）、traefik、prometheus、grafana、console 五个平台组件与 mysqld/redis 两个 exporter；dcn-app 新增结息批处理端点；seed 改为词汇表 + 确定性随机；verify 扩为 8 关。

**Tech Stack:** Go 1.22（`module dcn`，新增唯一依赖 `github.com/prometheus/client_golang`）、MySQL 8、RabbitMQ 3.13、Traefik v3、Prometheus、Grafana、docker compose。

## Global Constraints

- 任何文件（spec、plan、代码、README、注释）中不得出现特定银行机构名称；架构统一称为「DCN 架构」。
- 模板保持自包含 Go module：`make up && make seed && make verify` 一条链路跑通。
- 新依赖仅限 `github.com/prometheus/client_golang`（其余一律用标准库或既有依赖）。
- 注释与文档用中文（对齐模板现状）；标识符用英文。
- 每个任务完成后在 `templates/dcn` 下 `go build ./... && go test ./...` 必须绿。
- 工作目录：仓库根 `/Users/yuhaochen/Documents/codebase/projanvil/Jiade`；模板内路径均以 `templates/dcn/` 为前缀。

---

### Task 1: metrics 平台包（RED 埋点）

**Files:**
- Create: `templates/dcn/internal/platform/metrics/metrics.go`
- Test: `templates/dcn/internal/platform/metrics/metrics_test.go`
- Modify: `templates/dcn/go.mod`（经 `go get`/`go mod tidy`）

**Interfaces:**
- Produces:
  - `metrics.Mount(service string, h http.Handler) http.Handler` — 返回新 mux：`GET /metrics` 由 promhttp 处理，其余路径经 RED 中间件转发给 `h`。后续所有 cmd 入口用它包 handler。
  - `metrics.IncTx(status string)` — `rmb_tx_total{status}` 计数器 +1（Task 2 的 RMB 协调器用）。
  - 指标：`http_requests_total{service,handler,code}`、`http_request_duration_seconds{service,handler,code_class}` 直方图、`rmb_tx_total{status}`。

- [ ] **Step 1: 引入依赖**

```bash
cd templates/dcn
go get github.com/prometheus/client_golang@latest
go mod tidy
```

- [ ] **Step 2: 写失败测试**

`templates/dcn/internal/platform/metrics/metrics_test.go`：

```go
package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMountExposesMetricsAndCountsRequests(t *testing.T) {
	inner := http.NewServeMux()
	inner.HandleFunc("GET /accounts/{id}/balance", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := Mount("testsvc", inner)

	// 业务请求被计数
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/1001/balance", nil))
	if rec.Code != 200 {
		t.Fatalf("inner handler code = %d", rec.Code)
	}

	// /metrics 暴露且包含本服务计数；handler 标签用路由模板而非原始路径（防基数爆炸）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `http_requests_total{code="200",handler="/accounts/{id}/balance",service="testsvc"} 1`) {
		t.Fatalf("metrics missing request count:\n%s", body)
	}
	if !strings.Contains(body, `http_request_duration_seconds_count{code_class="2xx",handler="/accounts/{id}/balance",service="testsvc"} 1`) {
		t.Fatalf("metrics missing duration count:\n%s", body)
	}
}

func TestIncTx(t *testing.T) {
	IncTx("TESTSTATUS")
	rec := httptest.NewRecorder()
	Mount("x", http.NewServeMux()).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `rmb_tx_total{status="TESTSTATUS"} 1`) {
		t.Fatalf("missing rmb_tx_total:\n%s", rec.Body.String())
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `cd templates/dcn && go test ./internal/platform/metrics/`
Expected: FAIL（`package dcn/internal/platform/metrics: no Go files`）

- [ ] **Step 4: 实现**

`templates/dcn/internal/platform/metrics/metrics.go`：

```go
// Package metrics 提供统一的 Prometheus 埋点：RED 指标中间件、/metrics 端点、RMB 事务计数。
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	reqTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total", Help: "HTTP requests by service/handler/code.",
	}, []string{"service", "handler", "code"})
	reqDur = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP request latency by service/handler.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5},
	}, []string{"service", "handler", "code_class"})
	txTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "rmb_tx_total", Help: "RMB global transactions reaching a final status.",
	}, []string{"status"})
)

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// middleware 采集 RED 三要素。handler 标签取 ServeMux 路由后的 r.Pattern
// （如 /accounts/{id}/balance），避免按原始路径炸基数；必须在 next 之后读取。
func middleware(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		start := time.Now()
		next.ServeHTTP(sw, r)
		handler := r.Pattern
		if handler == "" {
			handler = "unmatched"
		}
		code := strconv.Itoa(sw.code)
		reqTotal.WithLabelValues(service, handler, code).Inc()
		reqDur.WithLabelValues(service, handler, code[:1]+"xx").Observe(time.Since(start).Seconds())
	})
}

// Mount 返回带 /metrics 端点与 RED 采集的总 handler；/metrics 自身不计数。
func Mount(service string, h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("/", middleware(service, h))
	return mux
}

// IncTx 记录一笔到达终态的 RMB 总事务（status: COMMITTED/COMPENSATED/FAILED）。
func IncTx(status string) { txTotal.WithLabelValues(status).Inc() }
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd templates/dcn && go test ./internal/platform/metrics/ -v`
Expected: 两个测试 PASS

- [ ] **Step 6: Commit**

```bash
git add templates/dcn/internal/platform/metrics templates/dcn/go.mod templates/dcn/go.sum
git commit -m "feat(dcn): prometheus RED metrics platform package"
```

---

### Task 2: 各服务接入 metrics

**Files:**
- Modify: `templates/dcn/cmd/gns/main.go`
- Modify: `templates/dcn/cmd/dcn-app/main.go`
- Modify: `templates/dcn/cmd/rmb-coordinator/main.go`
- Modify: `templates/dcn/cmd/adm/main.go`
- Modify: `templates/dcn/internal/dcnapp/server.go:38-48`（Handler 用 Mount 包装，/metrics 绕过限流）
- Modify: `templates/dcn/internal/rmb/coordinator.go:382-401`（advance 终态提交后 IncTx）

**Interfaces:**
- Consumes: `metrics.Mount`、`metrics.IncTx`（Task 1）
- Produces: 四个服务均暴露 `GET /metrics`；RMB 终态写入 `rmb_tx_total{status}`。

- [ ] **Step 1: gns 入口**

`templates/dcn/cmd/gns/main.go` 改为：

```go
package main

import (
	"dcn/internal/gns"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/redisx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	cache := redisx.Open(runx.MustEnv("REDIS_ADDR"))
	srv := gns.NewServer(db, cache)
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount("gns", srv.Handler()))
}
```

- [ ] **Step 2: adm / rmb-coordinator 入口**

`cmd/adm/main.go`：`runx.Serve(...)` 一行改为

```go
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount("adm", srv.Handler()))
```

`cmd/rmb-coordinator/main.go`：同样改为

```go
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount("rmb-coordinator", coord.Handler()))
```

（两文件均加 import `"dcn/internal/platform/metrics"`。）

- [ ] **Step 3: dcn-app —— /metrics 绕过限流**

`templates/dcn/internal/dcnapp/server.go` 的 `Handler()` 改为（import 加 `"dcn/internal/platform/metrics"`）：

```go
// Handler 返回带限流与 metrics 的路由；/metrics 与 /healthz 一样不受限流约束。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /accounts/{id}/balance", s.handleBalance)
	mux.HandleFunc("GET /internal/balance-sum", s.handleBalanceSum)
	mux.HandleFunc("POST /transfer", s.handleTransfer)
	return metrics.Mount(s.dcn, ratelimit.New(s.rps).Middleware(mux))
}
```

`cmd/dcn-app/main.go` 不变（Handler 内部完成包装）。

- [ ] **Step 4: RMB 终态计数**

`templates/dcn/internal/rmb/coordinator.go`：import 加 `"dcn/internal/platform/metrics"`。`advance` 中三处终态 `c.transition(...)` 后、`tx.Commit()` 成功处记录。最小改法——把三处

```go
			c.transition(tx, txID, "PROCESSING", "FAILED")
			return tx.Commit()
```

```go
			c.transition(tx, txID, "PROCESSING", "COMPENSATED")
			return tx.Commit()
```

（COMPENSATED 出现两次）以及

```go
	c.transition(tx, txID, "PROCESSING", "COMMITTED")
	return tx.Commit()
```

各自改为先 commit、成功后计数，例如 COMMITTED 处：

```go
	c.transition(tx, txID, "PROCESSING", "COMMITTED")
	if err := tx.Commit(); err != nil {
		return err
	}
	metrics.IncTx("COMMITTED")
	return nil
```

FAILED 与两处 COMPENSATED 同理（`metrics.IncTx("FAILED")` / `metrics.IncTx("COMPENSATED")`）。

- [ ] **Step 5: 构建与测试**

Run: `cd templates/dcn && go build ./... && go test ./...`
Expected: 全绿

- [ ] **Step 6: Commit**

```bash
git add templates/dcn/cmd templates/dcn/internal/dcnapp/server.go templates/dcn/internal/rmb/coordinator.go
git commit -m "feat(dcn): wire prometheus metrics into all services"
```

---

### Task 3: dcn-app 结息批处理

**Files:**
- Create: `templates/dcn/internal/dcnapp/interest.go`
- Test: `templates/dcn/internal/dcnapp/interest_test.go`
- Modify: `templates/dcn/internal/dcnapp/server.go:22-35`（Server 加 rate 字段、NewServer 加参、路由注册）
- Modify: `templates/dcn/cmd/dcn-app/main.go`（解析 `INTEREST_DAILY_RATE`）

**Interfaces:**
- Consumes: `applyMovement(tx *sql.Tx, txID string, accountID int, dir string, amt decimal.Decimal) error`（transfer.go 既有）
- Produces:
  - `InterestFor(balance, rate decimal.Decimal) decimal.Decimal` — 利息 = 余额×日利率，2 位小数 half-even 取舍
  - `interestTxID(bizDate string, accountID int) string` — journal 幂等键 `interest-<bizDate>-<accountId>`
  - `NewServer(dcn string, db *sql.DB, gns, rmb string, mqc *mq.Conn, rps float64, rate decimal.Decimal) *Server`（新签名）
  - HTTP：`POST /internal/batch/interest {"bizDate":"YYYY-MM-DD"}` → `{"dcn":"dcn01","accounts":N,"totalInterest":"12.34"}`

- [ ] **Step 1: 写失败测试**

`templates/dcn/internal/dcnapp/interest_test.go`：

```go
package dcnapp

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestInterestFor(t *testing.T) {
	rate := decimal.RequireFromString("0.0001")
	cases := []struct{ bal, want string }{
		{"1000.00", "0.10"},
		{"100000.00", "10.00"},
		{"12.34", "0"},      // 0.001234 -> 0.00，跳过入账
		{"9999.995", "1.00"}, // 0.9999995 -> 1.00
		{"15", "0"},          // 0.0015 -> 0.00（half-even 不进位）
		{"25", "0"},          // 0.0025 -> 0.00
		{"35", "0"},          // 0.0035 -> 0.00
	}
	for _, c := range cases {
		got := InterestFor(decimal.RequireFromString(c.bal), rate)
		if got.String() != c.want {
			t.Errorf("InterestFor(%s) = %s, want %s", c.bal, got, c.want)
		}
	}
}

func TestInterestTxID(t *testing.T) {
	if got := interestTxID("2026-08-02", 1001); got != "interest-2026-08-02-1001" {
		t.Fatalf("interestTxID = %s", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/dcn && go test ./internal/dcnapp/ -run 'TestInterest'`
Expected: FAIL（undefined: InterestFor）

- [ ] **Step 3: 实现 interest.go**

`templates/dcn/internal/dcnapp/interest.go`：

```go
package dcnapp

import (
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/shopspring/decimal"

	"dcn/internal/platform/httpx"
)

var bizDateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// InterestFor 计算单账户日终利息：余额×日利率，2 位小数 half-even 取舍。
func InterestFor(balance, rate decimal.Decimal) decimal.Decimal {
	return balance.Mul(rate).RoundBank(2)
}

// interestTxID 返回结息的 journal 幂等键（uk_tx_acct 兜底，重跑安全）。
func interestTxID(bizDate string, accountID int) string {
	return fmt.Sprintf("interest-%s-%d", bizDate, accountID)
}

type interestBatchRequest struct {
	BizDate string `json:"bizDate"`
}

// handleInterestBatch 日终结息：遍历本单元账户，逐账户独立本地事务入账
// （仿真生产批量按笔提交），每笔经 publishEvent 上报 ADM 全局镜像。
func (s *Server) handleInterestBatch(w http.ResponseWriter, r *http.Request) {
	var req interestBatchRequest
	if err := httpx.Decode(r, &req); err != nil || !bizDateRe.MatchString(req.BizDate) {
		httpx.Error(w, 400, "bizDate required in YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", req.BizDate); err != nil {
		httpx.Error(w, 400, "invalid bizDate")
		return
	}
	rows, err := s.db.Query(`SELECT account_id, balance FROM account ORDER BY account_id`)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	type acct struct {
		id  int
		bal string
	}
	var list []acct
	for rows.Next() {
		var a acct
		if err := rows.Scan(&a.id, &a.bal); err != nil {
			rows.Close()
			httpx.Error(w, 500, err.Error())
			return
		}
		list = append(list, a)
	}
	rows.Close()

	total := decimal.Zero
	count := 0
	for _, a := range list {
		bal, err := decimal.NewFromString(a.bal)
		if err != nil {
			continue
		}
		interest := InterestFor(bal, s.rate)
		if !interest.GreaterThan(decimal.Zero) {
			continue
		}
		txID := interestTxID(req.BizDate, a.id)
		tx, err := s.db.Begin()
		if err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		moveErr := applyMovement(tx, txID, a.id, "CREDIT", interest)
		if moveErr != nil {
			tx.Rollback()
			if moveErr == errDuplicate {
				continue // 重跑幂等：已入账的跳过
			}
			httpx.Error(w, 500, moveErr.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		s.publishEvent(txID, a.id, "CREDIT", interest)
		total = total.Add(interest)
		count++
	}
	httpx.JSON(w, 200, map[string]any{
		"dcn": s.dcn, "accounts": count, "totalInterest": total.String(),
	})
}
```

注意：`applyMovement` 返回的重复错误是包内 `errDuplicate`（transfer.go 定义），同包直接用；`s.db`/`s.rate` 来自 Server 结构体，无需额外 import。

- [ ] **Step 4: Server 加 rate 字段**

`templates/dcn/internal/dcnapp/server.go`：

```go
// Server 是一个 DCN 单元应用。
type Server struct {
	dcn  string
	db   *sql.DB
	gns  string
	rmb  string
	mqc  *mq.Conn
	rps  float64
	rate decimal.Decimal
	hc   *http.Client
}

// NewServer 构造 DCN 应用；rate 为日终结息日利率。
func NewServer(dcn string, db *sql.DB, gns, rmb string, mqc *mq.Conn, rps float64, rate decimal.Decimal) *Server {
	return &Server{dcn: dcn, db: db, gns: gns, rmb: rmb, mqc: mqc, rps: rps, rate: rate, hc: newHTTPClient()}
}
```

`Handler()` 的 mux 中加一行（`/internal/batch/interest` 走限流内侧即可）：

```go
	mux.HandleFunc("POST /internal/batch/interest", s.handleInterestBatch)
```

- [ ] **Step 5: 入口解析利率**

`templates/dcn/cmd/dcn-app/main.go`：

```go
package main

import (
	"strconv"

	"github.com/shopspring/decimal"

	"dcn/internal/dcnapp"
	"dcn/internal/platform/mq"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	dcnID := runx.MustEnv("DCN_ID")
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	mqc := mq.Dial(runx.MustEnv("AMQP_URL"))
	rps, err := strconv.ParseFloat(runx.Env("RATE_LIMIT_RPS", "200"), 64)
	if err != nil {
		rps = 200
	}
	rate, err := decimal.NewFromString(runx.Env("INTEREST_DAILY_RATE", "0.0001"))
	if err != nil || rate.IsNegative() {
		rate = decimal.RequireFromString("0.0001")
	}
	srv := dcnapp.NewServer(dcnID, db,
		runx.MustEnv("GNS_ENDPOINT"), runx.MustEnv("RMB_ENDPOINT"), mqc, rps, rate)
	srv.DeclareAndConsume()
	runx.Serve(":"+runx.Env("PORT", "8080"), srv.Handler())
}
```

- [ ] **Step 6: 构建与测试**

Run: `cd templates/dcn && go build ./... && go test ./internal/dcnapp/ -v`
Expected: 新旧测试全部 PASS（`InterestFor("9999.995")` 等边界以实际输出校准——若某 case 失败，以 `RoundBank` 真实语义修正测试期望值，并在代码注释中说明取舍规则）

- [ ] **Step 7: Commit**

```bash
git add templates/dcn/internal/dcnapp templates/dcn/cmd/dcn-app
git commit -m "feat(dcn): per-unit day-end interest batch endpoint"
```

---

### Task 4: batch-scheduler 服务

**Files:**
- Create: `templates/dcn/db/init/batch/01_init.sql`
- Create: `templates/dcn/internal/batch/scheduler.go`
- Test: `templates/dcn/internal/batch/scheduler_test.go`
- Create: `templates/dcn/cmd/batch-scheduler/main.go`

**Interfaces:**
- Consumes: GNS `GET /routes`（`[{dcn,segStart,segEnd,endpoint,status}]`）；dcn-app `POST /internal/batch/interest`（Task 3）
- Produces:
  - `batch.NewServer(db *sql.DB, gns string) *Server`，`(*Server).Handler() http.Handler`
  - `validateBizDate(s string) bool`
  - `(*Server).runOnUnits(ctx context.Context, bizDate string, units []route) []unitResult`
  - HTTP：`POST /jobs/interest {"bizDate":"YYYY-MM-DD"}` → `{"bizDate","status","totalInterest","units":[...]}`；`GET /jobs/{bizDate}` 同构响应
  - batch_db 表：`batch_job(biz_date PK, type, status, total_interest, created_at, finished_at)`、`batch_unit_result(biz_date, dcn, accounts, interest, status, error, PK(biz_date,dcn))`

- [ ] **Step 1: 建表 SQL**

`templates/dcn/db/init/batch/01_init.sql`：

```sql
-- 批量调度库
CREATE TABLE batch_job (
  biz_date       VARCHAR(10) PRIMARY KEY,
  type           VARCHAR(32) NOT NULL,
  status         VARCHAR(16) NOT NULL,           -- RUNNING/SUCCEEDED/FAILED
  total_interest DECIMAL(18,2) NOT NULL DEFAULT 0,
  created_at     TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  finished_at    TIMESTAMP NULL
);

CREATE TABLE batch_unit_result (
  biz_date VARCHAR(10) NOT NULL,
  dcn      VARCHAR(16) NOT NULL,
  accounts INT NOT NULL DEFAULT 0,
  interest DECIMAL(18,2) NOT NULL DEFAULT 0,
  status   VARCHAR(16) NOT NULL,                 -- DONE/FAILED
  error    VARCHAR(512) NULL,
  PRIMARY KEY (biz_date, dcn)
);
```

- [ ] **Step 2: 写失败测试**

`templates/dcn/internal/batch/scheduler_test.go`：

```go
package batch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestValidateBizDate(t *testing.T) {
	for _, ok := range []string{"2026-08-02", "2026-12-31"} {
		if !validateBizDate(ok) {
			t.Errorf("validateBizDate(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "2026-8-2", "2026-13-01", "2026-02-30", "today"} {
		if validateBizDate(bad) {
			t.Errorf("validateBizDate(%q) = true, want false", bad)
		}
	}
}

// runOnUnits 并发调用各单元结息端点并归集结果（单元失败不拖垮整体）。
func TestRunOnUnits(t *testing.T) {
	okUnit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dcn": "dcn01", "accounts": 50, "totalInterest": "5.00",
		})
	}))
	defer okUnit.Close()
	badUnit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer badUnit.Close()

	s := &Server{hc: okUnit.Client()}
	units := []route{
		{DCN: "dcn01", Endpoint: okUnit.URL, Status: "ACTIVE"},
		{DCN: "dcn02", Endpoint: badUnit.URL, Status: "ACTIVE"},
		{DCN: "dcn03", Endpoint: "http://127.0.0.1:1", Status: "ACTIVE"}, // 连接失败
	}
	results := s.runOnUnits(context.Background(), "2026-08-02", units)
	if len(results) != 3 {
		t.Fatalf("results len = %d", len(results))
	}
	if results[0].Err != nil || results[0].Accounts != 50 || results[0].Interest != "5.00" {
		t.Errorf("dcn01 result = %+v", results[0])
	}
	if results[1].Err == nil {
		t.Errorf("dcn02 should fail with HTTP 500")
	}
	if results[2].Err == nil {
		t.Errorf("dcn03 should fail with connection error")
	}
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `cd templates/dcn && go test ./internal/batch/`
Expected: FAIL（no Go files）

- [ ] **Step 4: 实现 scheduler.go**

`templates/dcn/internal/batch/scheduler.go`：

```go
// Package batch 实现独立批量调度服务：按业务日期发起日终批量（本期为结息），
// 并发调度各 DCN 单元、归集分单元结果、幂等重跑控制（仿真生产独立批量调度平台）。
package batch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"dcn/internal/platform/httpx"
)

const unitTimeout = 30 * time.Second

// Server 是批量调度服务。
type Server struct {
	db  *sql.DB
	gns string
	hc  *http.Client
}

// NewServer 构造调度服务。
func NewServer(db *sql.DB, gns string) *Server {
	return &Server{db: db, gns: gns, hc: &http.Client{Timeout: unitTimeout + 5*time.Second}}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /jobs/interest", s.handleCreateInterest)
	mux.HandleFunc("GET /jobs/{bizDate}", s.handleGetJob)
	return mux
}

// validateBizDate 校验 YYYY-MM-DD 且为真实日期。
func validateBizDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

type route struct {
	DCN      string `json:"dcn"`
	Endpoint string `json:"endpoint"`
	Status   string `json:"status"`
}

// unitResult 是一个单元的批量执行结果。
type unitResult struct {
	DCN      string
	Accounts int
	Interest string
	Err      error
}

func (s *Server) fetchActiveRoutes(ctx context.Context) ([]route, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", s.gns+"/routes", nil)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var all []route
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, err
	}
	var out []route
	for _, r := range all {
		if r.Status == "ACTIVE" {
			out = append(out, r)
		}
	}
	return out, nil
}

// runOnUnits 并发调各单元结息端点，按 units 顺序返回结果；单点失败记 Err 不中断。
func (s *Server) runOnUnits(ctx context.Context, bizDate string, units []route) []unitResult {
	results := make([]unitResult, len(units))
	var wg sync.WaitGroup
	for i, u := range units {
		wg.Add(1)
		go func(i int, u route) {
			defer wg.Done()
			results[i] = s.runOnUnit(ctx, bizDate, u)
		}(i, u)
	}
	wg.Wait()
	return results
}

func (s *Server) runOnUnit(ctx context.Context, bizDate string, u route) unitResult {
	res := unitResult{DCN: u.DCN}
	body, _ := json.Marshal(map[string]string{"bizDate": bizDate})
	ctx, cancel := context.WithTimeout(ctx, unitTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST",
		u.Endpoint+"/internal/batch/interest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.hc.Do(req)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		res.Err = fmt.Errorf("unit returned %d: %s", resp.StatusCode, raw)
		return res
	}
	var v struct {
		Accounts      int    `json:"accounts"`
		TotalInterest string `json:"totalInterest"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		res.Err = err
		return res
	}
	res.Accounts, res.Interest = v.Accounts, v.TotalInterest
	return res
}

type jobRequest struct {
	BizDate string `json:"bizDate"`
}

func (s *Server) handleCreateInterest(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	if err := httpx.Decode(r, &req); err != nil || !validateBizDate(req.BizDate) {
		httpx.Error(w, 400, "bizDate required in YYYY-MM-DD")
		return
	}
	var status string
	err := s.db.QueryRow(`SELECT status FROM batch_job WHERE biz_date = ?`, req.BizDate).Scan(&status)
	switch {
	case err == nil && status != "FAILED":
		s.respondJob(w, req.BizDate) // RUNNING/SUCCEEDED：幂等返回当前状态
		return
	case err == nil: // FAILED：允许重试，仅重跑失败单元（成功单元靠 journal 幂等兜底）
		if _, err := s.db.Exec(
			`UPDATE batch_job SET status = 'RUNNING', finished_at = NULL WHERE biz_date = ?`,
			req.BizDate); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.db.Exec(
			`INSERT INTO batch_job (biz_date, type, status) VALUES (?,'INTEREST','RUNNING')`,
			req.BizDate); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
	default:
		httpx.Error(w, 500, err.Error())
		return
	}

	units, err := s.fetchActiveRoutes(r.Context())
	if err != nil {
		s.finishJob(req.BizDate, "FAILED", "0")
		httpx.Error(w, 502, "gns unreachable: "+err.Error())
		return
	}
	results := s.runOnUnits(r.Context(), req.BizDate, units)
	total := "0"
	failed := false
	var sum float64
	for _, res := range results {
		st, errStr := "DONE", ""
		if res.Err != nil {
			st, errStr = "FAILED", res.Err.Error()
			failed = true
		}
		if _, err := s.db.Exec(
			`INSERT INTO batch_unit_result (biz_date, dcn, accounts, interest, status, error)
			 VALUES (?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE accounts=VALUES(accounts), interest=VALUES(interest),
			   status=VALUES(status), error=VALUES(error)`,
			req.BizDate, res.DCN, res.Accounts, res.Interest, st, errStr); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		if res.Err == nil {
			var f float64
			fmt.Sscanf(res.Interest, "%f", &f)
			sum += f
		}
	}
	total = fmt.Sprintf("%.2f", sum)
	if failed {
		s.finishJob(req.BizDate, "FAILED", total)
	} else {
		s.finishJob(req.BizDate, "SUCCEEDED", total)
	}
	s.respondJob(w, req.BizDate)
}

func (s *Server) finishJob(bizDate, status, total string) {
	_, _ = s.db.Exec(
		`UPDATE batch_job SET status = ?, total_interest = ?, finished_at = NOW() WHERE biz_date = ?`,
		status, total, bizDate)
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	s.respondJob(w, r.PathValue("bizDate"))
}

// respondJob 输出任务视图（状态 + 分单元明细）。
func (s *Server) respondJob(w http.ResponseWriter, bizDate string) {
	var status, total string
	err := s.db.QueryRow(
		`SELECT status, total_interest FROM batch_job WHERE biz_date = ?`, bizDate).
		Scan(&status, &total)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Error(w, 404, "job not found")
		return
	}
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	rows, err := s.db.Query(
		`SELECT dcn, accounts, interest, status, COALESCE(error,'') FROM batch_unit_result
		 WHERE biz_date = ? ORDER BY dcn`, bizDate)
	if err != nil {
		httpx.Error(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type unitView struct {
		DCN      string `json:"dcn"`
		Accounts int    `json:"accounts"`
		Interest string `json:"interest"`
		Status   string `json:"status"`
		Error    string `json:"error,omitempty"`
	}
	units := []unitView{}
	for rows.Next() {
		var u unitView
		if err := rows.Scan(&u.DCN, &u.Accounts, &u.Interest, &u.Status, &u.Error); err != nil {
			httpx.Error(w, 500, err.Error())
			return
		}
		units = append(units, u)
	}
	httpx.JSON(w, 200, map[string]any{
		"bizDate": bizDate, "type": "INTEREST", "status": status,
		"totalInterest": total, "units": units,
	})
}
```

`templates/dcn/cmd/batch-scheduler/main.go`：

```go
package main

import (
	"dcn/internal/batch"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/mysqlx"
	"dcn/internal/platform/runx"
)

func main() {
	db := mysqlx.Open(runx.MustEnv("DB_DSN"))
	srv := batch.NewServer(db, runx.MustEnv("GNS_ENDPOINT"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount("batch-scheduler", srv.Handler()))
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd templates/dcn && go build ./... && go test ./internal/batch/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add templates/dcn/db/init/batch templates/dcn/internal/batch templates/dcn/cmd/batch-scheduler
git commit -m "feat(dcn): standalone batch scheduler service with idempotent interest jobs"
```

---

### Task 5: compose 接入批量组件

**Files:**
- Modify: `templates/dcn/compose.yaml`（新增 batch-db、batch-scheduler 两个服务）
- Modify: `templates/dcn/test/topology.sh`（全局区服务断言列表补充）

**Interfaces:**
- Consumes: `cmd/batch-scheduler`（Task 4）
- Produces: `batch-scheduler` 容器（宿主机 18092）、`batch-db`（13313），均在 global-net。

- [ ] **Step 1: compose 加两个服务**

`templates/dcn/compose.yaml` 在 `adm` 服务块之后插入：

```yaml
  batch-db:
    <<: *mysql-common
    container_name: batch-db
    environment:
      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-dcn123}
      MYSQL_ROOT_HOST: "%"
      MYSQL_DATABASE: batch_db
    volumes:
      - ./db/init/batch:/docker-entrypoint-initdb.d:ro
      - batch-db-data:/var/lib/mysql
    ports: ["13313:3306"]
    networks: [global-net]

  batch-scheduler:
    <<: *app-health
    container_name: batch-scheduler
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: batch-scheduler }
    environment:
      DB_DSN: root:${MYSQL_ROOT_PASSWORD:-dcn123}@tcp(batch-db:3306)/batch_db?parseTime=true
      GNS_ENDPOINT: http://gns:8080
    ports: ["18092:8080"]
    networks: [global-net]
    depends_on:
      batch-db:
        condition: service_healthy
      gns:
        condition: service_started
```

`volumes:` 段加一行 `batch-db-data:`。

- [ ] **Step 2: seed --reset 覆盖 batch 表**

`templates/dcn/cmd/seed/main.go` 的 `resetAll()` 中 `dbs` map 加一行：

```go
		envOr("SEED_DSN_BATCH", "root:dcn123@tcp(127.0.0.1:13313)/batch_db"): {"batch_unit_result", "batch_job"},
```

- [ ] **Step 3: topology.sh 断言补充**

`templates/dcn/test/topology.sh` 把「全局区服务不接入任何 IDC 网络」一条改为：

```bash
check "全局区服务不接入任何 IDC 网络" \
  '[.services["gns"], .services["rmb-coordinator"], .services["adm"], .services["batch-scheduler"], .services["batch-db"]] | all(.networks | keys == ["global-net"])'
```

- [ ] **Step 4: 静态校验**

Run: `cd templates/dcn && docker compose config -q && bash test/topology.sh`
Expected: 无输出（config -q）+ `topology OK`

- [ ] **Step 5: Commit**

```bash
git add templates/dcn/compose.yaml templates/dcn/cmd/seed/main.go templates/dcn/test/topology.sh
git commit -m "feat(dcn): compose wiring for batch-db and batch-scheduler"
```

---

### Task 6: 真实规模 seed

**Files:**
- Modify: `templates/dcn/cmd/seed/main.go`（生成器化：词汇表 + 确定性随机余额）
- Test: `templates/dcn/cmd/seed/main_test.go`（新建）

**Interfaces:**
- Produces:
  - `personName(r *rand.Rand) string` — 中文姓名（词汇表拼接）
  - `initialBalance(r *rand.Rand, seg, i int) string` — 每单元前 2 户固定 `1000.00`（verify/README 依赖），其余 100.00–100000.00 确定性随机
  - 规模：dev=50/单元，full=2000/单元

- [ ] **Step 1: 写失败测试**

`templates/dcn/cmd/seed/main_test.go`：

```go
package main

import (
	"math/rand"
	"testing"
)

func TestPersonNameDeterministic(t *testing.T) {
	a := personName(rand.New(rand.NewSource(42)))
	b := personName(rand.New(rand.NewSource(42)))
	if a != b || a == "" {
		t.Fatalf("name not deterministic or empty: %q vs %q", a, b)
	}
}

func TestInitialBalance(t *testing.T) {
	// 每单元前 2 户固定 1000.00（verify 与 README 示例依赖）
	for _, i := range []int{0, 1} {
		if got := initialBalance(rand.New(rand.NewSource(1)), 1000, i); got != "1000.00" {
			t.Fatalf("fixed account %d balance = %s, want 1000.00", i, got)
		}
	}
	// 其余确定性随机，且在 [100, 100000] 区间
	r1, r2 := rand.New(rand.NewSource(7)), rand.New(rand.NewSource(7))
	for i := 2; i < 50; i++ {
		b1, b2 := initialBalance(r1, 1000, i), initialBalance(r2, 1000, i)
		if b1 != b2 {
			t.Fatalf("non-deterministic balance at %d: %s vs %s", i, b1, b2)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/dcn && go test ./cmd/seed/`
Expected: FAIL（undefined: personName）

- [ ] **Step 3: 重写 main.go**

`templates/dcn/cmd/seed/main.go` 主体改为（resetAll 不变，仅 dbs 已有 batch 行）：

```go
// seed 经 GNS 全流程开户灌入确定性测试数据（仿真生产的路由注册）。
// jiade CLI 硬编码调用：go run ./cmd/seed --scale=<dev|full> [--reset]
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
)

var (
	scale = flag.String("scale", "dev", "dev|full")
	reset = flag.Bool("reset", false, "clear all business data before seeding")
)

var surnames = []string{"赵", "钱", "孙", "李", "周", "吴", "郑", "王", "冯", "陈", "褚", "卫", "蒋", "沈", "韩", "杨"}

var givenNames = []string{"伟", "芳", "娜", "敏", "静", "磊", "军", "洋", "勇", "艳", "杰", "娟", "涛", "明", "超", "秀兰", "霞", "平", "刚", "桂英"}

// personName 用词汇表拼中文姓名。
func personName(r *rand.Rand) string {
	return surnames[r.Intn(len(surnames))] + givenNames[r.Intn(len(givenNames))]
}

// initialBalance 每单元前 2 户固定 1000.00（verify/README 依赖），其余 100–100000 随机。
func initialBalance(r *rand.Rand, seg, i int) string {
	if i < 2 {
		return "1000.00"
	}
	cents := 10000 + r.Int63n(9990000) // 100.00 ~ 100000.00（单位：分）
	return fmt.Sprintf("%.2f", float64(cents)/100)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	flag.Parse()
	counts := map[string]int{"dev": 50, "full": 2000}
	n, ok := counts[*scale]
	if !ok {
		log.Fatalf("unknown scale %q (want dev|full)", *scale)
	}
	if *reset {
		resetAll()
	}
	gns := envOr("GNS_ENDPOINT", "http://localhost:18080")
	hc := &http.Client{Timeout: 10 * time.Second}
	for _, seg := range []int{1000, 2000, 3000} {
		r := rand.New(rand.NewSource(int64(seg))) // 每单元确定性
		for i := 0; i < n; i++ {
			name := personName(r)
			bal := initialBalance(r, seg, i)
			body, _ := json.Marshal(map[string]string{
				"name":        name,
				"initBalance": bal,
				"requestId":   fmt.Sprintf("seed-%s-%d-%d", *scale, seg, i), // 幂等键
			})
			resp, err := hc.Post(gns+"/accounts", "application/json", bytes.NewReader(body))
			if err != nil {
				log.Fatalf("open account via GNS: %v", err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 300 {
				log.Fatalf("GNS returned %d: %s", resp.StatusCode, raw)
			}
			fmt.Printf("seeded: %s\n", raw)
		}
	}
	fmt.Println("seed done")
}
```

（`resetAll` 函数原样保留在文件尾部。）

- [ ] **Step 4: 跑测试确认通过**

Run: `cd templates/dcn && go test ./cmd/seed/ -v && go vet ./cmd/seed/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add templates/dcn/cmd/seed
git commit -m "feat(dcn): realistic-scale seed with name vocabulary and deterministic balances"
```

---

### Task 7: Traefik 统一接入层

**Files:**
- Modify: `templates/dcn/compose.yaml`（新增 traefik 服务；gns/adm/rmb-coordinator/batch-scheduler/dcnXX-app 打路由 labels）
- Modify: `templates/dcn/test/verify.sh:13-19, 63-77`（gate 1/2 改走网关）
- Modify: `templates/dcn/test/topology.sh`（traefik 网络断言）

**Interfaces:**
- Produces: 网关入口 `http://localhost:18070`：`/dcn/*`（LB 至各 dcn-app，剥前缀）、`/gns/*`、`/adm/*`、`/rmb/*`、`/batch/*`；dashboard `:18071`。

- [ ] **Step 1: compose 加 traefik 服务**

`compose.yaml` 全局区加：

```yaml
  traefik:
    image: traefik:v3.1
    container_name: dcn-traefik
    restart: unless-stopped
    command:
      - --providers.docker=true
      - --providers.docker.exposedbydefault=false
      - --providers.docker.network=dcn-global
      - --entrypoints.web.address=:80
      - --api.insecure=true
    ports:
      - "18070:80"    # 统一入口
      - "18071:8080"  # dashboard
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    networks: [global-net]
```

- [ ] **Step 2: 各服务打路由 labels**

dcn01-app / dcn02-app / dcn03-app / dcn04-app 四个服务各加（同一 service 名 `dcn` → 多容器合并为一个 LB 池）：

```yaml
    labels:
      traefik.enable: "true"
      traefik.http.routers.dcn.rule: PathPrefix(`/dcn`)
      traefik.http.routers.dcn.entrypoints: web
      traefik.http.routers.dcn.middlewares: dcn-strip
      traefik.http.middlewares.dcn-strip.stripprefix.prefixes: /dcn
      traefik.http.services.dcn.loadbalancer.server.port: "8080"
```

gns 加：

```yaml
    labels:
      traefik.enable: "true"
      traefik.http.routers.gns.rule: PathPrefix(`/gns`)
      traefik.http.routers.gns.entrypoints: web
      traefik.http.routers.gns.middlewares: gns-strip
      traefik.http.middlewares.gns-strip.stripprefix.prefixes: /gns
```

adm / rmb-coordinator / batch-scheduler 同构（前缀分别为 `/adm`、`/rmb`、`/batch`，middleware 名 `adm-strip`/`rmb-strip`/`batch-strip`；这三个服务不需要 `loadbalancer.server.port` label，默认取容器暴露端口 8080——为保险起见也统一加上 `traefik.http.services.<name>.loadbalancer.server.port: "8080"`，其中 service 名分别为 `adm`/`rmb`/`batch`）。

- [ ] **Step 3: verify gate 1/2 走网关**

`templates/dcn/test/verify.sh` 环境变量区加：

```bash
GATEWAY=${GATEWAY:-http://localhost:18070}
```

gate 1 的转账请求改为：

```bash
curl -sf -X POST "$GATEWAY/dcn/transfer" -H 'Content-Type: application/json' \
  -d '{"fromId":1001,"toId":1002,"amount":"100.00"}' >/dev/null \
  && pass "本地转账请求成功（经网关）" || fail "本地转账请求失败（经网关）"
```

gate 2 同理改为 `$GATEWAY/dcn/transfer`，pass/fail 文案加「（经网关）」。余额查询仍走直连端口（`balance $DCN01 1001` 不变）。

- [ ] **Step 4: topology.sh 断言**

加一条：

```bash
check "traefik 仅接入 global-net" \
  '.services["traefik"].networks | keys == ["global-net"]'
```

（同时把 traefik 加进「全局区服务不接入任何 IDC 网络」的列表。）

- [ ] **Step 5: 静态校验 + 联调冒烟**

Run: `cd templates/dcn && docker compose config -q && bash test/topology.sh`
Expected: `topology OK`

Run（需要栈已启动）：`docker compose up -d --build --wait traefik && curl -sf -X POST http://localhost:18070/dcn/transfer -H 'Content-Type: application/json' -d '{"fromId":1001,"toId":1002,"amount":"1.00"}'`
Expected: 返回 `{"status":"ok",...}`（若栈未启动，记录并在 Task 10 全量验收时补做）

- [ ] **Step 6: Commit**

```bash
git add templates/dcn/compose.yaml templates/dcn/test
git commit -m "feat(dcn): traefik unified access layer with per-zone routes"
```

---

### Task 8: Prometheus + exporter + Grafana

**Files:**
- Create: `templates/dcn/deploy/prometheus/prometheus.yml`
- Create: `templates/dcn/deploy/grafana/provisioning/datasources/prometheus.yaml`
- Create: `templates/dcn/deploy/grafana/provisioning/dashboards/dcn.yaml`
- Create: `templates/dcn/deploy/grafana/dashboards/dcn-red.json`
- Create: `templates/dcn/deploy/rabbitmq/enabled_plugins`
- Create: `templates/dcn/deploy/mysql-exporter/my.cnf`
- Modify: `templates/dcn/compose.yaml`（prometheus/grafana/mysqld-exporter/redis-exporter 服务；全部 Go 服务与 traefik 打 scrape labels；rabbitmq 挂 enabled_plugins）

**Interfaces:**
- Consumes: 各服务 `/metrics`（Task 1/2）
- Produces: Prometheus `:19090`（docker_sd 自动发现 + mysql probe 静态 job）；Grafana `:13000`（匿名 Viewer 免登，变量驱动 RED 仪表盘）。

- [ ] **Step 1: prometheus.yml**

`templates/dcn/deploy/prometheus/prometheus.yml`：

```yaml
global:
  scrape_interval: 5s

scrape_configs:
  # Go 服务 / traefik / redis-exporter / rabbitmq：label 驱动的 docker 自动发现
  - job_name: docker
    docker_sd_configs:
      - host: unix:///var/run/docker.sock
    relabel_configs:
      - source_labels: [__meta_docker_container_label_prometheus_scrape]
        regex: "true"
        action: keep
      - source_labels: [__meta_docker_container_name, __meta_docker_container_label_prometheus_port]
        separator: ":"
        regex: "/(.+):(\\d+)"
        replacement: "${1}:${2}"
        target_label: __address__
      - source_labels: [__meta_docker_container_name]
        regex: "/(.+)"
        replacement: "$1"
        target_label: container

  # MySQL 全部七库：mysqld-exporter /probe 多目标（库拓扑固定，静态列举；
  # mysqld-exporter 需同时挂 idc1/idc2/global-net 才能触达各库，见 Step 4）
  - job_name: mysql
    metrics_path: /probe
    static_configs:
      - targets:
          - dcn01-db:3306
          - dcn02-db:3306
          - dcn03-db:3306
          - gns-db:3306
          - rmb-db:3306
          - adm-db:3306
          - batch-db:3306
    relabel_configs:
      - source_labels: [__address__]
        target_label: __param_target
      - source_labels: [__param_target]
        target_label: instance
      - target_label: __address__
        replacement: mysqld-exporter:9104
```

- [ ] **Step 2: rabbitmq 启用内置 prometheus 插件**

`templates/dcn/deploy/rabbitmq/enabled_plugins`（注意结尾有点号）：

```
[rabbitmq_management,rabbitmq_prometheus].
```

`compose.yaml` 的 `dcn-rabbitmq` 服务 volumes 加一行：

```yaml
      - ./deploy/rabbitmq/enabled_plugins:/etc/rabbitmq/enabled_plugins:ro
```

并加 labels：

```yaml
    labels:
      prometheus.scrape: "true"
      prometheus.port: "15692"
```

- [ ] **Step 3: mysqld-exporter 配置**

`templates/dcn/deploy/mysql-exporter/my.cnf`：

```ini
[client]
user=root
password=dcn123
```

- [ ] **Step 4: compose 加观测组件 + scrape labels**

全部 Go 服务（gns、rmb-coordinator、adm、batch-scheduler、dcn01/02/03/04-app）与 traefik 加 labels（traefik 需额外开 metrics）：

```yaml
    labels:
      prometheus.scrape: "true"
      prometheus.port: "8080"
```

traefik 服务 command 加两项、labels 端口指向 metrics 入口：

```yaml
    command:
      # ...既有...
      - --metrics.prometheus=true
      - --entrypoints.metrics.address=:8082
      - --metrics.prometheus.entrypoint=metrics
    labels:
      prometheus.scrape: "true"
      prometheus.port: "8082"
```

新增服务：

```yaml
  prometheus:
    image: prom/prometheus:v2.54.1
    container_name: dcn-prometheus
    restart: unless-stopped
    volumes:
      - ./deploy/prometheus/prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - prometheus-data:/prometheus
    ports: ["19090:9090"]
    networks: [global-net]

  mysqld-exporter:
    image: prom/mysqld-exporter:v0.15.1
    container_name: mysqld-exporter
    restart: unless-stopped
    command:
      - --config.my-cnf=/cfg/my.cnf
    volumes:
      - ./deploy/mysql-exporter/my.cnf:/cfg/my.cnf:ro
    # 探测目标跨三个网络：dcn01/03-db 在 idc1，dcn02-db 在 idc2，其余在 global-net
    networks: [global-net, idc1, idc2]

  redis-exporter:
    image: oliver006/redis_exporter:v1.63.0
    container_name: redis-exporter
    restart: unless-stopped
    environment:
      REDIS_ADDR: redis://gns-redis:6379
    networks: [global-net]
    labels:
      prometheus.scrape: "true"
      prometheus.port: "9121"

  grafana:
    image: grafana/grafana:11.2.0
    container_name: dcn-grafana
    restart: unless-stopped
    environment:
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Viewer
      GF_AUTH_DISABLE_LOGIN_FORM: "true"
    volumes:
      - ./deploy/grafana/provisioning:/etc/grafana/provisioning:ro
      - ./deploy/grafana/dashboards:/var/lib/grafana/dashboards:ro
      - grafana-data:/var/lib/grafana
    ports: ["13000:3000"]
    networks: [global-net]
    depends_on:
      - prometheus
```

`volumes:` 段加 `prometheus-data:` 与 `grafana-data:`。

- [ ] **Step 5: Grafana provisioning**

`templates/dcn/deploy/grafana/provisioning/datasources/prometheus.yaml`：

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    uid: Prometheus   # 仪表盘 JSON 按 uid 引用，必须固定
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

`templates/dcn/deploy/grafana/provisioning/dashboards/dcn.yaml`：

```yaml
apiVersion: 1
providers:
  - name: dcn
    folder: DCN
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

- [ ] **Step 6: 变量驱动 RED 仪表盘**

`templates/dcn/deploy/grafana/dashboards/dcn-red.json`（变量 `service` 从 Prometheus 动态取，新服务自动可见）：

```json
{
  "uid": "dcn-red",
  "title": "DCN Services (RED)",
  "schemaVersion": 39,
  "version": 1,
  "refresh": "5s",
  "time": {"from": "now-15m", "to": "now"},
  "templating": {
    "list": [
      {
        "name": "service",
        "type": "query",
        "datasource": {"type": "prometheus", "uid": "Prometheus"},
        "query": "label_values(http_requests_total, service)",
        "refresh": 1,
        "includeAll": true,
        "multi": true,
        "current": {"selected": true, "text": ["All"], "value": ["$__all"]}
      }
    ]
  },
  "panels": [
    {
      "id": 1, "type": "timeseries", "title": "RPS",
      "datasource": {"type": "prometheus", "uid": "Prometheus"},
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 0},
      "targets": [{"expr": "sum by (service) (rate(http_requests_total{service=~\"$service\"}[1m]))", "legendFormat": "{{service}}", "refId": "A"}]
    },
    {
      "id": 2, "type": "timeseries", "title": "Error rate (5xx/s)",
      "datasource": {"type": "prometheus", "uid": "Prometheus"},
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 0},
      "targets": [{"expr": "sum by (service) (rate(http_requests_total{service=~\"$service\",code=~\"5..\"}[1m]))", "legendFormat": "{{service}}", "refId": "A"}]
    },
    {
      "id": 3, "type": "timeseries", "title": "P99 latency (s)",
      "datasource": {"type": "prometheus", "uid": "Prometheus"},
      "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
      "targets": [{"expr": "histogram_quantile(0.99, sum by (le, service) (rate(http_request_duration_seconds_bucket{service=~\"$service\"}[5m])))", "legendFormat": "{{service}}", "refId": "A"}]
    },
    {
      "id": 4, "type": "timeseries", "title": "RMB transactions (final status)",
      "datasource": {"type": "prometheus", "uid": "Prometheus"},
      "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
      "targets": [{"expr": "sum by (status) (increase(rmb_tx_total[5m]))", "legendFormat": "{{status}}", "refId": "A"}]
    },
    {
      "id": 5, "type": "stat", "title": "Infra up",
      "datasource": {"type": "prometheus", "uid": "Prometheus"},
      "gridPos": {"h": 6, "w": 24, "x": 0, "y": 16},
      "targets": [
        {"expr": "mysql_up", "legendFormat": "{{instance}}", "refId": "A"},
        {"expr": "up{job=\"docker\"}", "legendFormat": "{{container}}", "refId": "B"}
      ]
    }
  ]
}
```

- [ ] **Step 7: 静态校验 + 冒烟**

Run: `cd templates/dcn && docker compose config -q`
Expected: 无输出

Run（栈启动后）：
```bash
docker compose up -d --build --wait
curl -sf http://localhost:19090/api/v1/targets | jq '[.data.activeTargets[].labels.container] | unique'
curl -sf 'http://localhost:19090/api/v1/query?query=up' | jq '.data.result | length'
curl -sf http://localhost:13000/api/health
```
Expected: targets 含全部 Go 服务容器名；`up` 结果数 ≥ 10；grafana `{"database":"ok"}`

- [ ] **Step 8: Commit**

```bash
git add templates/dcn/deploy templates/dcn/compose.yaml
git commit -m "feat(dcn): prometheus docker_sd discovery, infra exporters, grafana RED dashboard"
```

---

### Task 9: console 观测服务

**Files:**
- Create: `templates/dcn/internal/console/server.go`
- Create: `templates/dcn/internal/console/index.html`
- Test: `templates/dcn/internal/console/server_test.go`
- Create: `templates/dcn/cmd/console/main.go`
- Modify: `templates/dcn/compose.yaml`（console 服务）
- Modify: `templates/dcn/test/topology.sh`

**Interfaces:**
- Consumes: Prometheus HTTP API（`/api/v1/targets`、`/api/v1/query`）；Docker Engine API（unix socket `/containers/json`）
- Produces:
  - `console.NewServer(promURL, dockerSocket string) *Server`，`(*Server).Handler() http.Handler`
  - HTTP：`GET /`（内嵌页面）；`GET /api/targets`（代理 Prometheus targets）；`GET /api/query?query=<promql>`（代理 instant query）；`GET /api/containers`（docker 容器列表：name/state/status）
  - console 容器：宿主机 18099。

- [ ] **Step 1: 写失败测试**

`templates/dcn/internal/console/server_test.go`：

```go
package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIndexServed(t *testing.T) {
	s := NewServer("http://prom:9090", "/nonexistent.sock")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "DCN") {
		t.Fatalf("index not served: code=%d", rec.Code)
	}
}

func TestQueryProxy(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/query") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "success"})
	}))
	defer prom.Close()
	s := NewServer(prom.URL, "/nonexistent.sock")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET",
		"/api/query?query=up", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "success") {
		t.Fatalf("query proxy failed: %d %s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/dcn && go test ./internal/console/`
Expected: FAIL（no Go files）

- [ ] **Step 3: 实现 server.go**

`templates/dcn/internal/console/server.go`：

```go
// Package console 实现观测台服务：内嵌单页（拓扑视图 + 状态墙 + RPS 曲线），
// 数据源为 Prometheus HTTP API 与 Docker Engine API（只读）。
package console

import (
	"context"
	_ "embed"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Server 是观测台服务。
type Server struct {
	prom   string
	hc     *http.Client
	docker *http.Client
}

// NewServer 构造观测台；dockerSocket 为 Docker Engine unix socket 路径。
func NewServer(promURL, dockerSocket string) *Server {
	return &Server{
		prom: promURL,
		hc:   &http.Client{Timeout: 5 * time.Second},
		docker: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", dockerSocket)
				},
			},
		},
	}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/targets", s.proxyProm("/api/v1/targets?state=active"))
	mux.HandleFunc("GET /api/query", s.handleQuery)
	mux.HandleFunc("GET /api/containers", s.handleContainers)
	return mux
}

// proxyProm 代理固定路径的 Prometheus API。
func (s *Server) proxyProm(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.proxy(w, s.prom+path)
	}
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, s.prom+"/api/v1/query?query="+url.QueryEscape(r.URL.Query().Get("query")))
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	resp, err := s.docker.Get("http://docker/containers/json?all=1")
	if err != nil {
		http.Error(w, `{"error":"docker unreachable"}`, 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) proxy(w http.ResponseWriter, target string) {
	resp, err := s.hc.Get(target)
	if err != nil {
		http.Error(w, `{"error":"upstream unreachable"}`, 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
```

- [ ] **Step 4: 实现 index.html**

`templates/dcn/internal/console/index.html`（纯原生 JS，每 2s 轮询；拓扑按 IDC 静态分组、状态点实时更新）：

```html
<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>DCN 观测台</title>
<style>
  body { font-family: -apple-system, "PingFang SC", sans-serif; margin: 24px; background: #0f1420; color: #dde3f0; }
  h1 { font-size: 20px; } h2 { font-size: 15px; color: #8fa3c8; margin-top: 24px; }
  .zone { border: 1px solid #2a3550; border-radius: 8px; padding: 12px 16px; margin-bottom: 12px; }
  .zone h3 { margin: 0 0 8px; font-size: 13px; color: #6f83ad; text-transform: uppercase; }
  .node { display: inline-flex; align-items: center; gap: 6px; background: #1a2338; border-radius: 6px;
          padding: 6px 10px; margin: 4px; font-size: 13px; }
  .dot { width: 9px; height: 9px; border-radius: 50%; background: #666; }
  .up { background: #3ecf6f; } .down { background: #e05252; }
  table { border-collapse: collapse; font-size: 13px; }
  td, th { border: 1px solid #2a3550; padding: 4px 10px; }
  .rps { font-variant-numeric: tabular-nums; }
  #err { color: #e05252; }
</style>
</head>
<body>
<h1>DCN 单元化架构 · 观测台</h1>
<div id="err"></div>

<h2>服务拓扑与状态</h2>
<div id="topology"></div>

<h2>流量（RPS, 1m 速率）</h2>
<table id="rps"><tr><th>service</th><th>rps</th></tr></table>

<h2>基础设施容器（Docker healthcheck）</h2>
<table id="infra"><tr><th>container</th><th>state</th><th>status</th></tr></table>

<script>
const ZONES = [
  { name: "global-net", nodes: ["gns", "rmb-coordinator", "adm", "batch-scheduler", "dcn-traefik", "dcn-prometheus", "dcn-grafana", "console"] },
  { name: "idc1", nodes: ["dcn01-app", "dcn03-app"] },
  { name: "idc2", nodes: ["dcn02-app", "dcn04-app"] },
];
const INFRA = ["gns-db", "gns-redis", "dcn-rabbitmq", "rmb-db", "adm-db", "batch-db",
               "dcn01-db", "dcn02-db", "dcn03-db", "dcn04-db"];

const topo = document.getElementById("topology");
for (const z of ZONES) {
  const div = document.createElement("div");
  div.className = "zone";
  div.innerHTML = `<h3>${z.name}</h3>` +
    z.nodes.map(n => `<span class="node"><span class="dot" id="dot-${n}"></span>${n}</span>`).join("");
  topo.appendChild(div);
}

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + " -> " + r.status);
  return r.json();
}

async function refresh() {
  try {
    const [targets, rps, containers] = await Promise.all([
      getJSON("/api/targets"),
      getJSON("/api/query?query=" + encodeURIComponent("sum by (service) (rate(http_requests_total[1m]))")),
      getJSON("/api/containers"),
    ]);
    document.getElementById("err").textContent = "";

    // 状态墙：Prometheus targets（key = container label）
    const health = {};
    for (const t of targets.data.activeTargets) health[t.labels.container] = t.health;
    // docker 容器兜底（grafana 等未被 scrape 的组件）
    const byName = {};
    for (const c of containers) byName[c.Names[0].replace(/^\//, "")] = c;
    for (const z of ZONES) for (const n of z.nodes) {
      const dot = document.getElementById("dot-" + n);
      let st = health[n];
      if (!st && byName[n]) st = byName[n].State === "running" ? "up" : "down";
      dot.className = "dot " + (st === "up" ? "up" : "down");
    }

    // RPS
    const tbl = document.getElementById("rps");
    tbl.innerHTML = "<tr><th>service</th><th>rps</th></tr>";
    for (const s of rps.data.result.sort((a, b) => a.metric.service < b.metric.service ? -1 : 1)) {
      tbl.innerHTML += `<tr><td>${s.metric.service}</td><td class="rps">${(+s.value[1]).toFixed(2)}</td></tr>`;
    }

    // 基础设施
    const inf = document.getElementById("infra");
    inf.innerHTML = "<tr><th>container</th><th>state</th><th>status</th></tr>";
    for (const name of INFRA) {
      const c = byName[name];
      inf.innerHTML += `<tr><td>${name}</td><td>${c ? c.State : "—"}</td><td>${c ? c.Status : "not running"}</td></tr>`;
    }
  } catch (e) {
    document.getElementById("err").textContent = "数据获取失败: " + e.message;
  }
}
refresh();
setInterval(refresh, 2000);
</script>
</body>
</html>
```

- [ ] **Step 5: 入口与 compose**

`templates/dcn/cmd/console/main.go`：

```go
package main

import (
	"dcn/internal/console"
	"dcn/internal/platform/metrics"
	"dcn/internal/platform/runx"
)

func main() {
	srv := console.NewServer(
		runx.Env("PROMETHEUS_URL", "http://prometheus:9090"),
		runx.Env("DOCKER_SOCKET", "/var/run/docker.sock"))
	runx.Serve(":"+runx.Env("PORT", "8080"), metrics.Mount("console", srv.Handler()))
}
```

`compose.yaml` 加：

```yaml
  console:
    <<: *app-health
    container_name: console
    build:
      context: .
      dockerfile: Dockerfile
      args: { SERVICE: console }
    environment:
      PROMETHEUS_URL: http://prometheus:9090
      DOCKER_SOCKET: /var/run/docker.sock
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    ports: ["18099:8080"]
    networks: [global-net]
    depends_on:
      prometheus:
        condition: service_started
    labels:
      prometheus.scrape: "true"
      prometheus.port: "8080"
```

`topology.sh` 的「全局区服务不接入任何 IDC 网络」列表追加 `.services["console"]`。

- [ ] **Step 6: 测试与构建**

Run: `cd templates/dcn && go build ./... && go test ./internal/console/ -v && docker compose config -q && bash test/topology.sh`
Expected: PASS + `topology OK`

- [ ] **Step 7: Commit**

```bash
git add templates/dcn/internal/console templates/dcn/cmd/console templates/dcn/compose.yaml templates/dcn/test/topology.sh
git commit -m "feat(dcn): observability console with topology wall, rps and infra status"
```

---

### Task 10: verify 第 8 关 + 文档 + 重新打包 + 全量验收

**Files:**
- Modify: `templates/dcn/test/verify.sh`（新增 gate 8；头部注释更新）
- Modify: `templates/dcn/.env.example`（补充可选环境变量）
- Modify: `templates/dcn/README.md`、`templates/dcn/README.zh-CN.md`（拓扑、组件表、端口表、快速上手、观测入口）
- Modify: `templates/dcn/ARCHITECTURE.md`（批量时序、观测体系、与生产差异清单）
- Modify: `internal/template/templates.tar`（`go generate` 产物）

**Interfaces:**
- Consumes: 网关 `/batch/*`（Task 7）、batch-scheduler API（Task 4）、ADM `/reconcile`（既有）

- [ ] **Step 1: verify gate 8**

`templates/dcn/test/verify.sh` 头部注释加 `# gate 8  日终批量（结息 + 幂等重跑 + ADM 核对）`。在 gate 7 之后、结尾汇总之前插入：

```bash
echo "== Gate 8: 日终批量（结息 + 幂等 + 核对）=="
BIZDATE=$(date +%F)
pre1=$(curl -sf "$DCN01/internal/balance-sum" | jq -r '.balanceSum')
pre2=$(curl -sf "$DCN02/internal/balance-sum" | jq -r '.balanceSum')
pre3=$(curl -sf "$DCN03/internal/balance-sum" | jq -r '.balanceSum')
job=$(curl -sf -X POST "$GATEWAY/batch/jobs/interest" -H 'Content-Type: application/json' \
  -d "{\"bizDate\":\"$BIZDATE\"}")
[ "$(echo "$job" | jq -r '.status')" = "SUCCEEDED" ] \
  && pass "结息批量执行成功 (SUCCEEDED)" || fail "结息批量失败: $job"
interest=$(echo "$job" | jq -r '.totalInterest')
unitsum=$(echo "$job" | jq -r '[.units[].interest | tonumber] | add | . * 100 | round / 100 | tostring')
assert_equal "$unitsum" "$interest" "分单元利息合计 = 调度器归集值"
# 余额变化 = 利息总额
post1=$(curl -sf "$DCN01/internal/balance-sum" | jq -r '.balanceSum')
post2=$(curl -sf "$DCN02/internal/balance-sum" | jq -r '.balanceSum')
post3=$(curl -sf "$DCN03/internal/balance-sum" | jq -r '.balanceSum')
pre_total=$(jq -nr --arg a "$pre1" --arg b "$pre2" --arg c "$pre3" '($a|tonumber)+($b|tonumber)+($c|tonumber) | tostring')
post_total=$(jq -nr --arg a "$post1" --arg b "$post2" --arg c "$post3" '($a|tonumber)+($b|tonumber)+($c|tonumber) | tostring')
assert_delta "$post_total" "$pre_total" "$interest" "三单元余额合计增加 = 利息总额"
# 幂等：同一 bizDate 重复触发不重跑，余额不再变
job2=$(curl -sf -X POST "$GATEWAY/batch/jobs/interest" -H 'Content-Type: application/json' \
  -d "{\"bizDate\":\"$BIZDATE\"}")
[ "$(echo "$job2" | jq -r '.totalInterest')" = "$interest" ] \
  && pass "重复触发幂等（总额不变）" || fail "重复触发结果漂移: $job2"
assert_equal "$post_total" "$(jq -nr --arg a "$(curl -sf "$DCN01/internal/balance-sum" | jq -r '.balanceSum')" --arg b "$(curl -sf "$DCN02/internal/balance-sum" | jq -r '.balanceSum')" --arg c "$(curl -sf "$DCN03/internal/balance-sum" | jq -r '.balanceSum')" '($a|tonumber)+($b|tonumber)+($c|tonumber) | tostring')" "重复触发后余额无二次入账"
sleep 3
rec=$(curl -sf "$ADM/reconcile")
[ "$(echo "$rec" | jq -r '.consistent')" = "true" ] \
  && pass "批量后 ADM 核对一致" || fail "批量后核对不一致: $rec"
```

注意 gate 8 在 gate 6（扩容 dcn04）之后执行：dcn04 已注册为 ACTIVE，batch 会调度到 dcn04（其余额为 500±转账结果，利息入账同样有效）——`assert_delta` 的三单元合计改为四单元，或前置说明：gate 8 把 dcn04 的 balance-sum 也计入 pre/post。实现时按四单元处理（`$DCN04/internal/balance-sum`），并在注释中写明。

- [ ] **Step 2: .env.example 补充**

`templates/dcn/.env.example` 追加：

```
# 结息日利率（dcn-app，默认 0.0001）
# INTEREST_DAILY_RATE=0.0001
```

- [ ] **Step 3: 文档更新**

- `README.md` / `README.zh-CN.md`：拓扑图加 traefik/prometheus/grafana/console/batch-* 节点；组件表加 batch-scheduler（日终批量调度）、traefik（统一接入层）、console（观测台）；端口表加 18070/18071/18092/18099/13000/19090/13313；快速上手加「打开 http://localhost:18099（观测台）、http://localhost:13000（Grafana RED 仪表盘）」；Hands-on 加结息批量 curl 示例（经网关 `POST /batch/jobs/interest`）；「与生产差异」清单加：生产批量调度有依赖编排与断点续跑，本仿真仅单任务类型；生产观测含告警与日志/链路，本仿真仅指标；前置要求注明容器数增至 19 个，建议 Docker Desktop 分配 ≥ 4GB 内存。
- `ARCHITECTURE.md`：§3 新增「3.4 日终批量时序」（客户端→网关→batch-scheduler→各单元并发结息→ADM 镜像→核对）；新增「观测体系」一节（RED 埋点 / docker_sd 发现 / Grafana 变量仪表盘 / console）；§7 表格加 gate 8 行。

- [ ] **Step 4: 全量单测**

Run: `cd templates/dcn && go build ./... && go test ./...`
Expected: 全绿

- [ ] **Step 5: 全量端到端验收**

Run: `cd templates/dcn && docker compose --profile expansion down -v --remove-orphans && make up && make seed && make verify && make topology-test`
Expected: `VERIFY OK` + `topology OK`（gate 1–8 全 PASS）

同时人工抽查：
```bash
curl -sf http://localhost:18099 | head -5                     # console 页面
curl -sf 'http://localhost:19090/api/v1/query?query=mysql_up' # MySQL 抓取
curl -sf http://localhost:13000/api/dashboards/uid/dcn-red    # Grafana 仪表盘已 provision
```

- [ ] **Step 6: 重新打包 templates.tar**

Run（仓库根）: `go generate ./internal/template && git status --short internal/template/`
Expected: `templates.tar` 有变更

- [ ] **Step 7: jiade 级别回归**

Run（仓库根）: `go build ./... && go test ./...`
Expected: 全绿

- [ ] **Step 8: Commit**

```bash
git add templates/dcn internal/template/templates.tar
git commit -m "feat(dcn): verify gate 8 (day-end batch), docs, observability entrypoints, re-embed tar"
```

---

## 任务依赖与执行顺序

Task 1 → Task 2（接线）→ Task 3（结息，独立于 2 可并行）→ Task 4（调度器）→ Task 5（compose 接入）→ Task 6（seed，独立可并行）→ Task 7（网关）→ Task 8（观测栈）→ Task 9（console）→ Task 10（收口验收）。其中 3/6 与 1/2 无依赖，但按序执行即可，每个任务独立可测、独立提交。
