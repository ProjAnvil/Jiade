# DCN 模板测试补齐 · 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 templates/dcn 四个服务（rmb/adm/gns/dcnapp）补齐 Go 单测，并新增 `//go:build integration` 外部集成测试包（从宿主机对运行中的栈发请求断言）。

**Architecture:** 依据 `docs/superpowers/specs/2026-08-02-dcn-test-coverage-design.md`。单测用可脚本化 fake driver（新包 `internal/platform/sqltest`，零外部依赖）+ httptest 假上游；集成测试独立包 `templates/dcn/test/integration/`，env 覆盖端点、栈不可达即 Skip。

**Tech Stack:** Go 1.22（module `dcn`），标准库 + 既有依赖（go-sql-driver 仅用于构造 `*mysql.MySQLError`），无新依赖。

## Global Constraints

- 任何文件（spec、plan、代码、README、注释）中不得出现特定银行机构名称；架构统一称为「DCN 架构」。
- 模板保持自包含 Go module：`make up && make seed && make verify` 一条链路跑通。
- 零新 Go 依赖（不引 testify/sqlmock/miniredis）。
- 注释与文档用中文（对齐模板现状）；标识符用英文。
- 单测不得依赖 docker 栈（`go test ./...` 在无栈环境必须全绿）；集成测试仅在 `-tags=integration` 下编译。
- verify.sh / topology.sh 逻辑零改动。
- 每个任务完成后在 `templates/dcn` 下 `go build ./... && go vet ./... && go test ./... && gofmt -l .`（无输出）必须绿。
- 工作目录：仓库根 `/Users/yuhaochen/Documents/codebase/projanvil/Jiade`；分支 feat/dcn-production-realism。

---

### Task 1: sqltest 共享测试替身包

**Files:**
- Create: `templates/dcn/internal/platform/sqltest/sqltest.go`
- Test: `templates/dcn/internal/platform/sqltest/sqltest_test.go`

**Interfaces:**
- Produces（后续 Task 2–5 全部依赖）:
  - `sqltest.Rule{Contains string; Max int; Columns []string; Rows [][]any; Err error; Result driver.Result}` — 按添加顺序匹配（Contains 为 SQL 子串），Max>0 时最多命中 Max 次；Rows 的元素必须是 driver.Value 合法类型（int64/float64/bool/[]byte/string/time.Time/nil）。
  - `sqltest.NewDB(t *testing.T, rules ...Rule) (*sql.DB, *Recorder)`
  - `(*Recorder).ExecCount(substr string) int`、`(*Recorder).LastExec(substr string) *Call`、`(*Recorder).QueryCount(substr string) int`
  - `sqltest.Call{Query string; Args []driver.Value}`
  - 行为约定：Query 匹配规则返回其 Columns/Rows（Rows 为空则 `QueryRow.Scan` 得 `sql.ErrNoRows`）；规则 Err 非 nil 时 Exec/Query 返回该错误；无任何规则匹配时报错 `sqltest: no rule for: <sql 前缀>`；`db.Begin()` 返回的 fakeTx 的 Commit/Rollback 恒为 nil，tx 内 Exec/Query 走同一套规则。

- [ ] **Step 1: 写失败测试**

`templates/dcn/internal/platform/sqltest/sqltest_test.go`：

```go
package sqltest

import (
	"database/sql/driver"
	"errors"
	"testing"
)

func TestRuleMatchingAndRecorder(t *testing.T) {
	db, rec := NewDB(t,
		Rule{Contains: "SELECT a", Max: 1, Columns: []string{"a"}},                    // 空集 → ErrNoRows
		Rule{Contains: "SELECT a", Columns: []string{"a"}, Rows: [][]any{{int64(7)}}}, // 第二次返回 7
		Rule{Contains: "UPDATE t", Result: nil},
	)
	var v int
	if err := db.QueryRow("SELECT a FROM t").Scan(&v); !errors.Is(err, errNoRowsForTest) && err == nil {
		t.Fatal("first query should be ErrNoRows")
	}
	if err := db.QueryRow("SELECT a FROM t").Scan(&v); err != nil || v != 7 {
		t.Fatalf("second query: v=%d err=%v", v, err)
	}
	if _, err := db.Exec("UPDATE t SET x=?", 1); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE t") != 1 || rec.QueryCount("SELECT a") != 2 {
		t.Fatalf("recorder: %+v", rec)
	}
	if rec.LastExec("UPDATE t").Args[0] != int64(1) {
		t.Fatalf("args not recorded: %+v", rec.LastExec("UPDATE t"))
	}
}

func TestRuleErrorAndTx(t *testing.T) {
	boom := errors.New("boom")
	db, _ := NewDB(t, Rule{Contains: "INSERT INTO j", Err: boom})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO j VALUES (1)"); !errors.Is(err, boom) {
		t.Fatalf("tx exec err = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestNoRulePanics(t *testing.T) {
	db, _ := NewDB(t)
	if _, err := db.Exec("SELECT 1"); err == nil {
		t.Fatal("unmatched SQL should error")
	}
}

var errNoRowsForTest = errors.New("sql: no rows in result set")
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/dcn && go test ./internal/platform/sqltest/`
Expected: FAIL（no Go files）。注：`errNoRowsForTest` 故意手搓（测试里用 `errors.Is` 比字符串更稳的可改为直接 import `database/sql` 用 `sql.ErrNoRows`——实现时把该 var 替换为 `sql.ErrNoRows` 并删除手写 var）。

- [ ] **Step 3: 实现 sqltest.go**

`templates/dcn/internal/platform/sqltest/sqltest.go`：

```go
// Package sqltest 提供可脚本化的 database/sql/driver 测试替身。
// 测试专用：供各服务包的单测共享，避免每个包各自手搓 fake driver。
// 零外部依赖；行为按 Rule 列表顺序匹配（Contains 为 SQL 子串）。
package sqltest

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Rule 是一条 SQL 匹配规则；Max>0 时最多命中 Max 次，随后落入后续规则。
type Rule struct {
	Contains string
	Max      int
	Columns  []string
	Rows     [][]any
	Err      error
	Result   driver.Result
}

// Call 记录一次 Exec/Query 调用。
type Call struct {
	Query string
	Args  []driver.Value
}

// Recorder 线程安全地记录全部调用。
type Recorder struct {
	mu      sync.Mutex
	execs   []Call
	queries []Call
}

// ExecCount 返回 Query 文本含 substr 的 Exec 次数。
func (r *Recorder) ExecCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.execs {
		if strings.Contains(c.Query, substr) {
			n++
		}
	}
	return n
}

// QueryCount 返回 Query 文本含 substr 的 Query 次数。
func (r *Recorder) QueryCount(substr string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.queries {
		if strings.Contains(c.Query, substr) {
			n++
		}
	}
	return n
}

// LastExec 返回最后一条匹配的 Exec，未命中返回 nil。
func (r *Recorder) LastExec(substr string) *Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.execs) - 1; i >= 0; i-- {
		if strings.Contains(r.execs[i].Query, substr) {
			c := r.execs[i]
			return &c
		}
	}
	return nil
}

type ruleState struct {
	rule Rule
	hits int
}

type fakeDriver struct {
	rules *[]*ruleState
	rec   *Recorder
}

func (d fakeDriver) Open(string) (driver.Conn, error) {
	return &conn{rules: d.rules, rec: d.rec}, nil
}

type conn struct {
	rules *[]*ruleState
	rec   *Recorder
}

func (c *conn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("sqltest: Prepare unsupported")
}
func (c *conn) Close() error { return nil }

func (c *conn) Begin() (driver.Tx, error) { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

func (c *conn) match(query string) (*ruleState, error) {
	for _, rs := range *c.rules {
		if rs.rule.Max > 0 && rs.hits >= rs.rule.Max {
			continue
		}
		if strings.Contains(query, rs.rule.Contains) {
			rs.hits++
			return rs, nil
		}
	}
	prefix := query
	if len(prefix) > 60 {
		prefix = prefix[:60]
	}
	return nil, fmt.Errorf("sqltest: no rule for: %s", prefix)
}

func (c *conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	rs, err := c.match(query)
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.execs = append(c.rec.execs, Call{query, args})
	c.rec.mu.Unlock()
	if rs.rule.Err != nil {
		return nil, rs.rule.Err
	}
	if rs.rule.Result != nil {
		return rs.rule.Result, nil
	}
	return result(1), nil
}

func (c *conn) Query(query string, args []driver.Value) (driver.Rows, error) {
	rs, err := c.match(query)
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.queries = append(c.rec.queries, Call{query, args})
	c.rec.mu.Unlock()
	if rs.rule.Err != nil {
		return nil, rs.rule.Err
	}
	return &rows{cols: rs.rule.Columns, data: rs.rule.Rows}, nil
}

type result int64

func (r result) LastInsertId() (int64, error) { return 0, nil }
func (r result) RowsAffected() (int64, error) { return int64(r), nil }

type rows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.data) {
		return io.EOF
	}
	row := r.data[r.pos]
	r.pos++
	for i := range dest {
		if i < len(row) {
			dest[i] = driver.Value(row[i])
		}
	}
	return nil
}

var seq atomic.Int64

// NewDB 注册唯一驱动名并返回 *sql.DB 与 Recorder。
func NewDB(t *testing.T, rules ...Rule) (*sql.DB, *Recorder) {
	t.Helper()
	rec := &Recorder{}
	states := make([]*ruleState, len(rules))
	for i, r := range rules {
		states[i] = &ruleState{rule: r}
	}
	name := fmt.Sprintf("sqltest-%d", seq.Add(1))
	sql.Register(name, fakeDriver{rules: &states, rec: rec})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, rec
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd templates/dcn && go test ./internal/platform/sqltest/ -v && go vet ./... && gofmt -l .`
Expected: 3 个测试 PASS（`errNoRowsForTest` 已按 Step 2 注替换为 `sql.ErrNoRows`），vet/gofmt 干净。

- [ ] **Step 5: Commit**

```bash
git add templates/dcn/internal/platform/sqltest
git commit -m "test(dcn): scriptable fake sql driver shared by service unit tests"
```

---

### Task 2: rmb 协调器单测

**Files:**
- Modify: `templates/dcn/internal/rmb/coordinator.go`（可测性小重构：publishFn 字段）
- Test: `templates/dcn/internal/rmb/coordinator_test.go`（新建）

**Interfaces:**
- Consumes: `sqltest.NewDB/Rule/Recorder`（Task 1）
- Produces: `Coordinator.publishFn func(exchange, key string, body []byte) error`（NewCoordinator 内默认 `mqc.Publish`；`publishPending` 改用它且 nil 时直接返回）

- [ ] **Step 1: 可测性重构（先改实现，保持现有测试绿）**

`coordinator.go` 的 `Coordinator` struct 加字段：

```go
	attempts  sync.Map // 补偿步骤重试计数："txID:stepNo" -> int
	publishFn func(exchange, key string, body []byte) error // 默认可注入，测试替身
```

`NewCoordinator` 返回前加：`coord.publishFn = mqc.Publish`（构造后赋值）。
`publishPending` 中 `c.mqc.Publish(...)` 改为 `c.publishFn(...)`，函数开头加：

```go
	if c.publishFn == nil {
		return // 单测未注入时不投递
	}
```

Run: `cd templates/dcn && go build ./... && go test ./...` — 全绿（行为不变）。

- [ ] **Step 2: 写测试**

`templates/dcn/internal/rmb/coordinator_test.go`：

```go
package rmb

import (
	"database/sql/driver"
	"encoding/json"
	"strings"
	"testing"

	"dcn/internal/contracts"
	"dcn/internal/platform/sqltest"
)

func stepPayload(t *testing.T, txID string, stepNo int, action string) string {
	t.Helper()
	b, _ := json.Marshal(contracts.StepMessage{
		TxID: txID, StepNo: stepNo, Action: action, AccountID: 1001, Amount: "50.00",
	})
	return string(b)
}

// register：txId 已存在时幂等返回当前状态，不重复落库。
func TestRegisterIdempotent(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "SELECT status FROM tx_log", Columns: []string{"status"},
			Rows: [][]any{{"COMMITTED"}}},
	)
	c := &Coordinator{db: db}
	txID, status, err := c.register(txRequest{TxID: "tx-1", Type: "TRANSFER",
		Steps: []txRequestStep{{DCN: "dcn01", Action: "DEBIT", AccountID: 1001, Amount: "50.00"}}})
	if err != nil || txID != "tx-1" || status != "COMMITTED" {
		t.Fatalf("register = (%s, %s, %v)", txID, status, err)
	}
	if rec.ExecCount("INSERT INTO tx_log") != 0 {
		t.Fatal("idempotent register must not insert")
	}
}

// advance：全部 DONE → COMMITTED。
func TestAdvanceAllDoneCommits(t *testing.T) {
	p1 := stepPayload(t, "t1", 1, "DEBIT")
	p2 := stepPayload(t, "t1", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "DONE", p2},
			}},
		sqltest.Rule{Contains: "UPDATE tx_log SET status"},
	)
	c := &Coordinator{db: db}
	if err := c.advance("t1"); err != nil {
		t.Fatal(err)
	}
	upd := rec.LastExec("UPDATE tx_log SET status")
	if upd == nil || upd.Args[0] != "COMMITTED" {
		t.Fatalf("want COMMITTED transition, got %+v", upd)
	}
}

// advance：一步 FAILED → 为 DONE 的原始步骤逆序补建 COMPENSATE_* 并投递。
func TestAdvanceFailedStepCreatesCompensation(t *testing.T) {
	p1 := stepPayload(t, "t2", 1, "DEBIT")
	p2 := stepPayload(t, "t2", 2, "CREDIT")
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", p1},
				{int64(2), "dcn02", "CREDIT", "FAILED", p2},
			}},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
		sqltest.Rule{Contains: "SELECT step_no, dcn, payload",
			Columns: []string{"step_no", "dcn", "payload"},
			Rows:    [][]any{{int64(3), "dcn01", stepPayload(t, "t2", 3, "COMPENSATE_DEBIT")}}},
	)
	var published []string
	c := &Coordinator{db: db, publishFn: func(exchange, key string, body []byte) error {
		published = append(published, string(body))
		return nil
	}}
	if err := c.advance("t2"); err != nil {
		t.Fatal(err)
	}
	ins := rec.LastExec("INSERT INTO tx_step_log")
	if ins == nil || ins.Args[2] != "COMPENSATE_DEBIT" {
		t.Fatalf("want COMPENSATE_DEBIT step inserted, got %+v", ins)
	}
	if len(published) != 1 || !strings.Contains(published[0], "COMPENSATE_DEBIT") {
		t.Fatalf("compensation not published: %v", published)
	}
}

// handleReceipt：迟到 DONE 回执（步骤已 FAILED、事务已 COMPENSATED）→ 重开事务并补发反向补偿。
func TestLateDoneReceiptReopensAndRecompensates(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM tx_step_log WHERE tx_id = ? AND step_no = ?",
			Columns: []string{"status"}, Rows: [][]any{{"FAILED"}}},
		sqltest.Rule{Contains: "SELECT status FROM tx_log WHERE tx_id = ?", Max: 1,
			Columns: []string{"status"}, Rows: [][]any{{"COMPENSATED"}}},
		sqltest.Rule{Contains: "UPDATE tx_step_log SET status = 'DONE'"},
		sqltest.Rule{Contains: "UPDATE tx_log SET status = 'PROCESSING'", Result: result1()},
		sqltest.Rule{Contains: "FOR UPDATE", Columns: []string{"status"}, Rows: [][]any{{"PROCESSING"}}},
		sqltest.Rule{Contains: "step_no, dcn, action, status, payload",
			Columns: []string{"step_no", "dcn", "action", "status", "payload"},
			Rows: [][]any{
				{int64(1), "dcn01", "DEBIT", "DONE", stepPayload(t, "t3", 1, "DEBIT")},
				{int64(2), "dcn02", "CREDIT", "DONE", stepPayload(t, "t3", 2, "CREDIT")},
				{int64(3), "dcn01", "COMPENSATE_DEBIT", "DONE", stepPayload(t, "t3", 3, "COMPENSATE_DEBIT")},
			}},
		sqltest.Rule{Contains: "INSERT INTO tx_step_log"},
		sqltest.Rule{Contains: "SELECT step_no, dcn, payload",
			Columns: []string{"step_no", "dcn", "payload"},
			Rows:    [][]any{{int64(4), "dcn02", stepPayload(t, "t3", 4, "COMPENSATE_CREDIT")}}},
	)
	var published []string
	c := &Coordinator{db: db, publishFn: func(exchange, key string, body []byte) error {
		published = append(published, string(body))
		return nil
	}}
	rc, _ := json.Marshal(contracts.Receipt{TxID: "t3", StepNo: 2, DCN: "dcn02", Status: "DONE"})
	if err := c.handleReceipt(rc); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("UPDATE tx_log SET status = 'PROCESSING'") != 1 {
		t.Fatal("tx should be reopened to PROCESSING")
	}
	ins := rec.LastExec("INSERT INTO tx_step_log")
	if ins == nil || ins.Args[2] != "COMPENSATE_CREDIT" {
		t.Fatalf("want COMPENSATE_CREDIT inserted for late DONE, got %+v", ins)
	}
	if len(published) == 0 {
		t.Fatal("re-compensation not published")
	}
}

func result1() driver.Result { return oneRow{} }

type oneRow struct{}

func (oneRow) LastInsertId() (int64, error) { return 0, nil }
func (oneRow) RowsAffected() (int64, error) { return 1, nil }
```

注意：测试直接构造 `&Coordinator{db: db, publishFn: ...}`（字段均小写，同包可访问）。`register` 里 `SELECT status FROM tx_log WHERE tx_id = ?` 与 `FOR UPDATE` 规则的匹配顺序——sqltest 按规则顺序+子串匹配，`FOR UPDATE` 规则不会误吞普通 status 查询（子串不同）。迟到回执用例中 step3 已 DONE 的 COMPENSATE_DEBIT 与两个 DONE 原始步骤构成「2 done originals - 1 comp = 1 missing」，advance 补建 COMPENSATE_CREDIT——与生产代码 advance 的计数逻辑一致。

- [ ] **Step 3: 跑测试确认通过**

Run: `cd templates/dcn && go test ./internal/rmb/ -v && go vet ./... && gofmt -l .`
Expected: 4 个测试 PASS。若某用例失败，先核对规则子串与生产 SQL 文本（`grep -n "SELECT\|UPDATE\|INSERT" internal/rmb/coordinator.go`），以生产 SQL 为准修规则，不改生产逻辑。

- [ ] **Step 4: Commit**

```bash
git add templates/dcn/internal/rmb
git commit -m "test(dcn): rmb coordinator state machine unit tests (register/advance/late receipt)"
```

---

### Task 3: adm 单测

**Files:**
- Test: `templates/dcn/internal/adm/server_test.go`（新建）

**Interfaces:**
- Consumes: `sqltest`（Task 1）；`mysqlx.IsDuplicate` 依赖 `*mysql.MySQLError{Number:1062}`（go-sql-driver 为既有依赖，测试可 import）。

- [ ] **Step 1: 写测试**

`templates/dcn/internal/adm/server_test.go`：

```go
package adm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"dcn/internal/contracts"
	"dcn/internal/platform/sqltest"
)

func eventBody(t *testing.T, txID string) []byte {
	t.Helper()
	b, _ := json.Marshal(contracts.BalanceEvent{
		TxID: txID, AccountID: 1001, DCN: "dcn01", Direction: "CREDIT", Amount: "10.00",
	})
	return b
}

// handleEvent：重复事件经 uk_event 去重，global_balance 只加一次。
func TestHandleEventIdempotent(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO event_log", Max: 1}, // 首次成功
		sqltest.Rule{Contains: "INSERT INTO event_log",
			Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}}, // 重复
		sqltest.Rule{Contains: "INSERT INTO global_balance"},
	)
	s := NewServer(db, "http://gns-unused")
	if err := s.handleEvent(eventBody(t, "tx-dup")); err != nil {
		t.Fatal(err)
	}
	if err := s.handleEvent(eventBody(t, "tx-dup")); err != nil {
		t.Fatal(err)
	}
	if rec.ExecCount("INSERT INTO global_balance") != 1 {
		t.Fatalf("mirror should be updated exactly once, got %d",
			rec.ExecCount("INSERT INTO global_balance"))
	}
}

// handleReconcile：ADM 镜像合计 == 各单元实时合计 → consistent=true。
func TestReconcileConsistent(t *testing.T) {
	unit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"dcn": "dcn01", "accounts": 2, "balanceSum": "100.00",
		})
	}))
	defer unit.Close()
	gns := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]routeView{{DCN: "dcn01", Endpoint: unit.URL, Status: "ACTIVE"}})
	}))
	defer gns.Close()

	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "FROM global_balance",
			Columns: []string{"COALESCE(SUM(balance), 0)"}, Rows: [][]any{{"100.00"}}},
	)
	s := NewServer(db, gns.URL)
	rec := httptest.NewRecorder()
	s.handleReconcile(rec, httptest.NewRequest("GET", "/reconcile", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var v struct {
		Consistent bool   `json:"consistent"`
		AdmTotal   string `json:"admTotal"`
		DcnTotal   string `json:"dcnTotal"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Consistent || v.AdmTotal != "100.00" || v.DcnTotal != "100.00" {
		t.Fatalf("reconcile = %+v", v)
	}
	if !strings.Contains(rec.Body.String(), "dcn01") {
		t.Fatal("perDcn should include dcn01")
	}
}
```

注意：adm `handleReconcile` 中 `routeView` 是同包类型，测试同包直接用。fake GNS 返回的 JSON 字段名需与 `routeView` 的 json tag 一致（dcn/endpoint/status）。

- [ ] **Step 2: 跑测试确认通过**

Run: `cd templates/dcn && go test ./internal/adm/ -v && go vet ./... && gofmt -l .`
Expected: 2 个测试 PASS。

- [ ] **Step 3: Commit**

```bash
git add templates/dcn/internal/adm
git commit -m "test(dcn): adm event idempotency and reconcile unit tests"
```

---

### Task 4: gns 单测

**Files:**
- Test: `templates/dcn/internal/gns/server_test.go`（新建）

**Interfaces:**
- Consumes: `sqltest`（Task 1）。Redis：`gns.NewServer(db, cache)` 需要 `*redis.Client`——测试用 `redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: time.Millisecond, ReadTimeout: time.Millisecond, WriteTimeout: time.Millisecond, MaxRetries: -1})`（连接失败被现有代码忽略，不阻塞）。

- [ ] **Step 1: 写测试**

`templates/dcn/internal/gns/server_test.go`：

```go
package gns

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"dcn/internal/platform/sqltest"
)

func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: "127.0.0.1:1", DialTimeout: time.Millisecond,
		ReadTimeout: time.Millisecond, WriteTimeout: time.Millisecond, MaxRetries: -1,
	})
}

func openReq(body string) *http.Request {
	return httptest.NewRequest("POST", "/accounts", strings.NewReader(body))
}

// requestId 命中：直接返回首次开户结果，不再分配新账号、不调 DCN。
func TestOpenAccountIdempotentByRequestID(t *testing.T) {
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "WHERE ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"},
			Rows:    [][]any{{int64(1001), "dcn01", "http://dcn01-app:8080"}}},
	)
	s := NewServer(db, deadRedis())
	recorder := httptest.NewRecorder()
	s.handleOpenAccount(recorder, openReq(`{"name":"张三","initBalance":"100.00","requestId":"r-1"}`))
	if recorder.Code != 200 {
		t.Fatalf("status = %d (body: %s)", recorder.Code, recorder.Body)
	}
	var v LocateResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &v); err != nil || v.AccountID != 1001 {
		t.Fatalf("resp = %s", recorder.Body)
	}
	if rec.ExecCount("INSERT INTO account_route") != 0 {
		t.Fatal("idempotent hit must not allocate")
	}
}

// DCN 建户失败：回滚路由行并返回 502，保证路由与实体一致。
func TestOpenAccountRollsBackRouteOnDCNFailure(t *testing.T) {
	dcn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer dcn.Close()

	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "WHERE ar.request_id = ?",
			Columns: []string{"account_id", "dcn", "endpoint"}}, // 空集 → 未命中
		sqltest.Rule{Contains: "FROM route_segment ORDER BY seg_start",
			Columns: []string{"dcn", "seg_start", "seg_end", "endpoint", "status"},
			Rows:    [][]any{{"dcn01", int64(1000), int64(1999), dcn.URL, "ACTIVE"}}},
		sqltest.Rule{Contains: "GROUP BY dcn", Columns: []string{"dcn", "COUNT(*)"}}, // 空计数
		sqltest.Rule{Contains: "MAX(account_id)",
			Columns: []string{"MAX(account_id)"}, Rows: [][]any{{nil}}}, // 空号段
		sqltest.Rule{Contains: "INSERT INTO account_route"},
		sqltest.Rule{Contains: "DELETE FROM account_route"},
	)
	s := NewServer(db, deadRedis())
	recorder := httptest.NewRecorder()
	s.handleOpenAccount(recorder, openReq(`{"name":"李四","initBalance":"100.00","requestId":"r-2"}`))
	if recorder.Code != 502 {
		t.Fatalf("status = %d, want 502 (body: %s)", recorder.Code, recorder.Body)
	}
	if rec.ExecCount("DELETE FROM account_route") != 1 {
		t.Fatal("route row should be rolled back")
	}
	del := rec.LastExec("DELETE FROM account_route")
	if del.Args[0] != int64(1001) {
		t.Fatalf("rollback args = %v, want account_id 1001", del.Args)
	}
}
```

注意：`NextAccountID` 对空号段返回 `SegStart+1`（1001），DELETE 参数应为 int64(1001)。MAX 查询返回一行 nil（`sql.NullInt64.Valid=false`）——sqltest 的 `[][]any{{nil}}` 即一行一列 NULL。

- [ ] **Step 2: 跑测试确认通过**

Run: `cd templates/dcn && go test ./internal/gns/ -v && go vet ./... && gofmt -l .`
Expected: 新旧测试全部 PASS。

- [ ] **Step 3: Commit**

```bash
git add templates/dcn/internal/gns
git commit -m "test(dcn): gns open-account idempotency and rollback unit tests"
```

---

### Task 5: dcnapp 单测

**Files:**
- Modify: `templates/dcn/internal/dcnapp/server.go`（可测性小重构：publishFn 字段）
- Modify: `templates/dcn/internal/dcnapp/steps.go`（回执发布改走 publishFn）
- Test: `templates/dcn/internal/dcnapp/server_test.go`（新建）

**Interfaces:**
- Consumes: `sqltest`（Task 1）
- Produces: `Server.publishFn func(exchange, key string, body []byte) error`（NewServer 内默认 `mqc.Publish`；`publishEvent` 与 `handleStep` 改用它且 nil 时跳过发布）

- [ ] **Step 1: 可测性重构**

`server.go` 的 `Server` struct 加字段：

```go
	publishFn func(exchange, key string, body []byte) error // 默认 mqc.Publish，测试可注入
```

`NewServer` 返回字面量加 `publishFn: mqc.Publish`。`publishEvent` 中 `s.mqc.Publish("adm.events", "", evt)` 改为：

```go
	if s.publishFn == nil {
		return // 单测未注入时不发布
	}
	if err := s.publishFn("adm.events", "", evt); err != nil {
```

`steps.go` 的 `handleStep` 中 `s.mqc.Publish("", "rmb.receipts", receipt)` 改为 `s.publishFn(...)`，调用前加同样的 nil 守卫（守卫时视为发布成功直接返回 nil，避免 requeue 语义干扰测试）。

Run: `cd templates/dcn && go build ./... && go test ./...` — 全绿。

- [ ] **Step 2: 写测试**

`templates/dcn/internal/dcnapp/server_test.go`：

```go
package dcnapp

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/shopspring/decimal"

	"dcn/internal/platform/sqltest"
)

func newSrv(dcnID string, db *sql.DB, gnsURL, rmbURL string) *Server {
	return &Server{
		dcn: dcnID, db: db, gns: gnsURL, rmb: rmbURL,
		rps: 1000, rate: decimal.RequireFromString("0.0001"), hc: newHTTPClient(),
	}
}

func locateReply(dcn, endpoint string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"accountId": 1, "dcn": dcn, "endpoint": endpoint,
		})
	}
}

// 本单元转账：GNS 双方同单元 → 本地事务，200 ok。
func TestTransferLocalSameUnit(t *testing.T) {
	gns := httptest.NewServer(locateReply("dcn01", "http://dcn01-app:8080"))
	defer gns.Close()
	db, rec := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO journal"},
		sqltest.Rule{Contains: "balance - ?"},
		sqltest.Rule{Contains: "balance + ?"},
	)
	s := newSrv("dcn01", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, httptest.NewRequest("POST", "/transfer",
		strings.NewReader(`{"fromId":1001,"toId":1002,"amount":"10.00"}`)))
	if w.Code != 200 {
		t.Fatalf("status = %d (body: %s)", w.Code, w.Body)
	}
	if rec.ExecCount("INSERT INTO journal") != 2 {
		t.Fatalf("want 2 journal entries, got %d", rec.ExecCount("INSERT INTO journal"))
	}
}

// 跨单元转账：提交 RMB 且 COMMITTED → 200 ok。
func TestTransferCrossUnitViaRMB(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /locate", func(w http.ResponseWriter, r *http.Request) {
		dcn, ep := "dcn01", "http://dcn01-app:8080"
		if r.URL.Query().Get("accountId") == "2001" {
			dcn, ep = "dcn02", "http://dcn02-app:8080"
		}
		json.NewEncoder(w).Encode(map[string]any{"accountId": 1, "dcn": dcn, "endpoint": ep})
	})
	gns := httptest.NewServer(mux)
	defer gns.Close()
	rmb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"txId": "tx-9", "status": "COMMITTED"})
	}))
	defer rmb.Close()

	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn01", db, gns.URL, rmb.URL)
	w := httptest.NewRecorder()
	s.handleTransfer(w, httptest.NewRequest("POST", "/transfer",
		strings.NewReader(`{"fromId":1001,"toId":2001,"amount":"10.00"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "tx-9") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
}

// 接入层转发：源账户在他单元 → 原样转发到其 endpoint。
func TestTransferForwardsToSourceUnit(t *testing.T) {
	dcn01 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","txId":"fwd-1"}`))
	}))
	defer dcn01.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /locate", func(w http.ResponseWriter, r *http.Request) {
		ep := "http://dcn02-app:8080"
		dcn := "dcn02"
		if r.URL.Query().Get("accountId") == "1001" {
			dcn, ep = "dcn01", dcn01.URL
		}
		json.NewEncoder(w).Encode(map[string]any{"accountId": 1, "dcn": dcn, "endpoint": ep})
	})
	gns := httptest.NewServer(mux)
	defer gns.Close()

	db, _ := sqltest.NewDB(t)
	s := newSrv("dcn02", db, gns.URL, "http://rmb-unused")
	w := httptest.NewRecorder()
	s.handleTransfer(w, httptest.NewRequest("POST", "/transfer",
		strings.NewReader(`{"fromId":1001,"toId":2002,"amount":"10.00"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "fwd-1") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
}

// 重复开户：MySQL 1062 → 200 exists。
func TestCreateAccountDuplicateReturnsExists(t *testing.T) {
	db, _ := sqltest.NewDB(t,
		sqltest.Rule{Contains: "INSERT INTO account",
			Err: &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}},
	)
	s := newSrv("dcn01", db, "http://gns-unused", "http://rmb-unused")
	s.publishFn = func(string, string, []byte) error { return nil }
	w := httptest.NewRecorder()
	s.handleCreateAccount(w, httptest.NewRequest("POST", "/accounts",
		strings.NewReader(`{"accountId":1001,"name":"王五","initBalance":"100.00"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "exists") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
}
```

注意：GNS locate 响应里 accountId 字段值不重要（handleTransfer 只用 DCN/Endpoint）；`newSrv` 需要 import `database/sql`（已含在上面的 import 块中）。

- [ ] **Step 3: 跑测试确认通过**

Run: `cd templates/dcn && go test ./internal/dcnapp/ -v && go vet ./... && gofmt -l .`
Expected: 新旧测试全部 PASS。

- [ ] **Step 4: Commit**

```bash
git add templates/dcn/internal/dcnapp
git commit -m "test(dcn): dcnapp transfer routing and account creation unit tests"
```

---

### Task 6: 外部集成测试包（//go:build integration）

**Files:**
- Create: `templates/dcn/test/integration/helper_test.go`
- Create: `templates/dcn/test/integration/gns_test.go`
- Create: `templates/dcn/test/integration/dcnapp_test.go`
- Create: `templates/dcn/test/integration/rmb_test.go`
- Create: `templates/dcn/test/integration/adm_test.go`
- Create: `templates/dcn/test/integration/batch_test.go`
- Create: `templates/dcn/test/integration/gateway_test.go`
- Create: `templates/dcn/test/integration/metrics_test.go`
- Create: `templates/dcn/test/integration/console_test.go`

**Interfaces:**
- Consumes: 运行中的 docker 栈（`make up`，宿主机端口 18070–18099）
- Produces: `make integration-test` 可运行的测试包；helper：
  - `envOr(key, def string) string`
  - `probe(t *testing.T, url string)` — GET `<url>/healthz` 失败即 `t.Skip`
  - `doJSON(t *testing.T, method, url string, body any) (int, []byte)`
  - `openAccount(t *testing.T, gnsBase, name, requestID string) (accountID int, dcn string)`
  - `openPair(t *testing.T, gnsBase string, sameUnit bool) (aID, bID int, dcnA, dcnB string)` — 最多开 8 个账户，按 dcn 分组找一对
  - `unitBase(dcn string) string` — dcn01→18081、dcn02→18082、dcn03→18083、dcn04→18084
  - 常量：`gnsBase/rmbBase/admBase/batchBase/consoleBase/gatewayBase`（env 覆盖，默认 localhost 端口）

- [ ] **Step 1: helper_test.go**

`templates/dcn/test/integration/helper_test.go`：

```go
//go:build integration

// Package integration 是从宿主机对运行中的 docker 栈发起的外部集成测试。
// 前提：make up（栈不可达即 Skip）。端点可用环境变量覆盖（DCN_IT_<NAME>）。
// 运行：go test -tags=integration -p 1 ./test/integration/...
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	gatewayBase = envOr("DCN_IT_GATEWAY", "http://localhost:18070")
	gnsBase     = envOr("DCN_IT_GNS", "http://localhost:18080")
	rmbBase     = envOr("DCN_IT_RMB", "http://localhost:18090")
	admBase     = envOr("DCN_IT_ADM", "http://localhost:18091")
	batchBase   = envOr("DCN_IT_BATCH", "http://localhost:18092")
	consoleBase = envOr("DCN_IT_CONSOLE", "http://localhost:18099")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// unitBase 把单元名映射为宿主机端口（locate 返回的 endpoint 是容器内地址，宿主机不可达）。
func unitBase(dcn string) string {
	switch dcn {
	case "dcn01":
		return envOr("DCN_IT_DCN01", "http://localhost:18081")
	case "dcn02":
		return envOr("DCN_IT_DCN02", "http://localhost:18082")
	case "dcn03":
		return envOr("DCN_IT_DCN03", "http://localhost:18083")
	case "dcn04":
		return envOr("DCN_IT_DCN04", "http://localhost:18084")
	}
	return ""
}

var hc = &http.Client{Timeout: 10 * time.Second}

// probe 探测服务健康端点，不可达即 Skip（栈未启动时不报错）。
func probe(t *testing.T, url string) {
	t.Helper()
	resp, err := hc.Get(url + "/healthz")
	if err != nil {
		t.Skipf("stack not running (%s): %v", url, err)
	}
	resp.Body.Close()
}

func doJSON(t *testing.T, method, url string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// openAccount 经 GNS 开户（requestID 幂等），返回账号与所在单元。
func openAccount(t *testing.T, gnsURL, name, requestID string) (int, string) {
	t.Helper()
	code, raw := doJSON(t, "POST", gnsURL+"/accounts", map[string]string{
		"name": name, "initBalance": "1000.00", "requestId": requestID,
	})
	if code >= 300 {
		t.Fatalf("open account: %d %s", code, raw)
	}
	var v struct {
		AccountID int    `json:"accountId"`
		DCN       string `json:"dcn"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.AccountID, v.DCN
}

// openPair 开若干账户，找一对同单元（sameUnit=true）或跨单元（false）账户。
func openPair(t *testing.T, gnsURL string, sameUnit bool) (int, int, string, string) {
	t.Helper()
	type acct struct {
		id  int
		dcn string
	}
	var list []acct
	tag := fmt.Sprintf("itest-pair-%d", time.Now().UnixNano())
	for i := 0; i < 8; i++ {
		id, dcn := openAccount(t, gnsURL, "集成测试", fmt.Sprintf("%s-%d", tag, i))
		for _, a := range list {
			if (a.dcn == dcn) == sameUnit {
				return a.id, id, a.dcn, dcn
			}
		}
		list = append(list, acct{id, dcn})
	}
	t.Fatalf("no suitable account pair after 8 opens (sameUnit=%v)", sameUnit)
	return 0, 0, "", ""
}

// balance 读账户余额（字符串原样返回）。
func balance(t *testing.T, dcn string, accountID int) string {
	t.Helper()
	code, raw := doJSON(t, "GET",
		fmt.Sprintf("%s/accounts/%d/balance", unitBase(dcn), accountID), nil)
	if code != 200 {
		t.Fatalf("balance: %d %s", code, raw)
	}
	var v struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Balance
}

// balanceSum 读单元余额合计。
func balanceSum(t *testing.T, dcn string) string {
	t.Helper()
	code, raw := doJSON(t, "GET", unitBase(dcn)+"/internal/balance-sum", nil)
	if code != 200 {
		t.Fatalf("balance-sum %s: %d", dcn, code)
	}
	var v struct {
		Sum string `json:"balanceSum"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.Sum
}

// decEq 用浮点容差比较两个十进制字符串。
func decEq(a, b string) bool {
	var x, y float64
	if _, err := fmt.Sscanf(a, "%f", &x); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(b, "%f", &y); err != nil {
		return false
	}
	d := x - y
	return d*d < 0.000001
}

func decAdd(a, b string) string {
	var x, y float64
	fmt.Sscanf(a, "%f", &x)
	fmt.Sscanf(b, "%f", &y)
	return fmt.Sprintf("%.2f", x+y)
}

// contains 断言 body 含子串。
func contains(t *testing.T, raw []byte, substr string) {
	t.Helper()
	if !strings.Contains(string(raw), substr) {
		t.Fatalf("response missing %q: %s", substr, raw)
	}
}
```

- [ ] **Step 2: gns_test.go**

`templates/dcn/test/integration/gns_test.go`：

```go
//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// GNS 外部行为：未开户 404 → 开户 → locate 命中 → 同 requestId 幂等返回同账号。
func TestGNSOpenAndLocate(t *testing.T) {
	probe(t, gnsBase)
	code, _ := doJSON(t, "GET", gnsBase+"/locate?accountId=999999999", nil)
	if code != 404 {
		t.Fatalf("locate unknown = %d, want 404", code)
	}
	rid := fmt.Sprintf("itest-gns-%d", time.Now().UnixNano())
	id, dcn := openAccount(t, gnsBase, "集成测试", rid)
	if id <= 0 || dcn == "" {
		t.Fatalf("open = (%d, %s)", id, dcn)
	}
	code, raw := doJSON(t, "GET", fmt.Sprintf("%s/locate?accountId=%d", gnsBase, id), nil)
	if code != 200 {
		t.Fatalf("locate = %d", code)
	}
	contains(t, raw, `"dcn":"`+dcn+`"`)
	id2, _ := openAccount(t, gnsBase, "集成测试", rid)
	if id2 != id {
		t.Fatalf("idempotent open: %d != %d", id2, id)
	}
}
```

- [ ] **Step 3: dcnapp_test.go**

`templates/dcn/test/integration/dcnapp_test.go`：

```go
//go:build integration

package integration

import (
	"testing"
)

// 本单元转账：同单元两账户，余额差值精确。
func TestLocalTransferDelta(t *testing.T) {
	probe(t, unitBase("dcn01"))
	a, b, dcnA, _ := openPair(t, gnsBase, true)
	beforeA := balance(t, dcnA, a)
	code, raw := doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"fromId": a, "toId": b, "amount": "10.00",
	})
	if code != 200 {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	contains(t, raw, `"txId"`)
	if !decEq(balance(t, dcnA, a), decAdd(beforeA, "-10.00")) {
		t.Fatal("from balance delta mismatch")
	}
}
```

- [ ] **Step 4: rmb_test.go**

`templates/dcn/test/integration/rmb_test.go`：

```go
//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// 跨单元转账经 RMB：COMMITTED + 两步骤 DONE + 同 txId 重放幂等（余额只变一次）。
func TestCrossUnitTransferAndIdempotentReplay(t *testing.T) {
	probe(t, rmbBase)
	a, b, dcnA, dcnB := openPair(t, gnsBase, false)
	txID := fmt.Sprintf("itest-rmb-%d", time.Now().UnixNano())
	beforeB := balance(t, dcnB, b)

	code, raw := doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"txId": txID, "fromId": a, "toId": b, "amount": "20.00",
	})
	if code != 200 {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	code, raw = doJSON(t, "GET", rmbBase+"/transactions/"+txID, nil)
	if code != 200 {
		t.Fatalf("get tx: %d", code)
	}
	contains(t, raw, `"status":"COMMITTED"`)
	contains(t, raw, `"status":"DONE"`)

	// 同 txId 重放：RMB 幂等返回，余额不二次变动
	code, _ = doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"txId": txID, "fromId": a, "toId": b, "amount": "20.00",
	})
	if code != 200 {
		t.Fatal("replay should still succeed (idempotent)")
	}
	time.Sleep(time.Second)
	if !decEq(balance(t, dcnB, b), decAdd(beforeB, "20.00")) {
		t.Fatal("replay must not double-credit")
	}
}
```

- [ ] **Step 5: adm_test.go**

`templates/dcn/test/integration/adm_test.go`：

```go
//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// ADM 核对：转账事件汇总后 reconcile 最终 consistent。
func TestReconcileEventuallyConsistent(t *testing.T) {
	probe(t, admBase)
	a, b, dcnA, _ := openPair(t, gnsBase, true)
	if code, raw := doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"fromId": a, "toId": b, "amount": "1.00",
	}); code != 200 {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	for i := 0; i < 10; i++ {
		code, raw := doJSON(t, "GET", admBase+"/reconcile", nil)
		if code == 200 {
			var v struct {
				Consistent bool `json:"consistent"`
			}
			if json.Unmarshal(raw, &v) == nil && v.Consistent {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("reconcile not consistent within 10s")
}
```

- [ ] **Step 6: batch_test.go**

`templates/dcn/test/integration/batch_test.go`：

```go
//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// 日终批量（昨日 bizDate，避开 verify gate 8 的当日任务）：
// SUCCEEDED → 重复触发幂等（totalInterest 不变、余额合计无二次入账）。
func TestInterestBatchIdempotent(t *testing.T) {
	probe(t, batchBase)
	openAccount(t, gnsBase, "集成测试", "itest-batch-seed") // 保证至少一户
	bizDate := time.Now().AddDate(0, 0, -1).Format("2006-01-02")

	code, raw := doJSON(t, "POST", gatewayBase+"/batch/jobs/interest",
		map[string]string{"bizDate": bizDate})
	if code != 200 {
		t.Fatalf("batch trigger: %d %s", code, raw)
	}
	var job struct {
		Status        string `json:"status"`
		TotalInterest string `json:"totalInterest"`
	}
	if err := json.Unmarshal(raw, &job); err != nil || job.Status != "SUCCEEDED" {
		t.Fatalf("job = %s", raw)
	}
	sum1 := decAdd(decAdd(balanceSum(t, "dcn01"), balanceSum(t, "dcn02")), balanceSum(t, "dcn03"))

	code, raw = doJSON(t, "POST", gatewayBase+"/batch/jobs/interest",
		map[string]string{"bizDate": bizDate})
	if code != 200 {
		t.Fatalf("retrigger: %d", code)
	}
	var job2 struct {
		TotalInterest string `json:"totalInterest"`
	}
	json.Unmarshal(raw, &job2)
	if !decEq(job2.TotalInterest, job.TotalInterest) {
		t.Fatal("retrigger totalInterest drifted")
	}
	sum2 := decAdd(decAdd(balanceSum(t, "dcn01"), balanceSum(t, "dcn02")), balanceSum(t, "dcn03"))
	if !decEq(sum1, sum2) {
		t.Fatal("retrigger credited interest twice")
	}
}
```

- [ ] **Step 7: gateway_test.go**

`templates/dcn/test/integration/gateway_test.go`：

```go
//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// 网关接入层：前缀剥离 + /dcn/* LB 落任意单元仍完成本单元转账（透明转发）。
func TestGatewayRoutes(t *testing.T) {
	probe(t, gatewayBase)
	rid := fmt.Sprintf("itest-gw-%d", time.Now().UnixNano())
	id, dcn := openAccount(t, gatewayBase+"/gns", "集成测试", rid)
	code, raw := doJSON(t, "GET",
		fmt.Sprintf("%s/gns/locate?accountId=%d", gatewayBase, id), nil)
	if code != 200 {
		t.Fatalf("gateway locate: %d %s", code, raw)
	}
	contains(t, raw, `"dcn":"`+dcn+`"`)

	a, b, dcnA, _ := openPair(t, gatewayBase+"/gns", true)
	beforeA := balance(t, dcnA, a)
	code, raw = doJSON(t, "POST", gatewayBase+"/dcn/transfer", map[string]any{
		"fromId": a, "toId": b, "amount": "1.00",
	})
	if code != 200 {
		t.Fatalf("gateway transfer: %d %s", code, raw)
	}
	if !decEq(balance(t, dcnA, a), decAdd(beforeA, "-1.00")) {
		t.Fatal("gateway transfer balance delta mismatch")
	}
}
```

- [ ] **Step 8: metrics_test.go**

`templates/dcn/test/integration/metrics_test.go`：

```go
//go:build integration

package integration

import (
	"strings"
	"testing"
)

// 各服务 /metrics 暴露本服务 RED 指标（CounterVec 首次计数才产出序列，故先发一条已埋点请求）。
func TestMetricsEndpoints(t *testing.T) {
	services := []struct {
		name string
		base string
		hit  string // 已埋点的请求路径（/healthz 未埋点，不可用）
	}{
		{"gns", gnsBase, "/locate?accountId=1"},
		{"rmb-coordinator", rmbBase, "/transactions/nonexistent"},
		{"adm", admBase, "/report/summary"},
		{"batch-scheduler", batchBase, "/jobs/1900-01-01"},
		{"dcn01", unitBase("dcn01"), "/accounts/1/balance"},
		{"dcn02", unitBase("dcn02"), "/accounts/1/balance"},
		{"dcn03", unitBase("dcn03"), "/accounts/1/balance"},
		{"console", consoleBase, "/api/targets"},
	}
	for _, s := range services {
		probe(t, s.base)
		doJSON(t, "GET", s.base+s.hit, nil) // 触发计数（404 也计入）
		_, raw := doJSON(t, "GET", s.base+"/metrics", nil)
		if !strings.Contains(string(raw), `http_requests_total{service="`+s.name+`"`) {
			t.Fatalf("%s /metrics missing its series", s.name)
		}
	}
}
```

注意：metrics_test.go 的 import 中 `strings` 与 `testing` 够用；`contains` helper 也可用，但这里需要拼接 service 名，直接 `strings.Contains` 更清晰。

- [ ] **Step 9: console_test.go**

`templates/dcn/test/integration/console_test.go`：

```go
//go:build integration

package integration

import (
	"strings"
	"testing"
)

// console 代理端点：targets（Prometheus）与 containers（Docker API）可达且返回合法 JSON。
func TestConsoleProxies(t *testing.T) {
	probe(t, consoleBase)
	code, raw := doJSON(t, "GET", consoleBase+"/api/targets", nil)
	if code != 200 || !strings.Contains(string(raw), `"status":"success"`) {
		t.Fatalf("targets: %d %s", code, raw)
	}
	code, raw = doJSON(t, "GET", consoleBase+"/api/containers", nil)
	if code != 200 || !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		t.Fatalf("containers: %d %.200s", code, raw)
	}
}
```

- [ ] **Step 10: 验证（无栈与有栈两种形态）**

无栈（CI 静态形态）：
Run: `cd templates/dcn && go vet -tags=integration ./test/integration/ && go test -tags=integration -p 1 ./test/integration/`
Expected: 编译通过；无栈时全部 Skip（`ok ... [no tests ran]` 或全部 SKIP，退出码 0）。

有栈（留待 Task 7 全量验收真跑）。

- [ ] **Step 11: Commit**

```bash
git add templates/dcn/test/integration
git commit -m "test(dcn): external integration suite hitting the running stack (build-tagged)"
```

---

### Task 7: 工程接线 + 全量验收

**Files:**
- Modify: `templates/dcn/Makefile`（integration-test target）
- Modify: `.github/workflows/ci.yml`（dcn-e2e job 加一步）
- Modify: `templates/dcn/README.md`、`templates/dcn/README.zh-CN.md`（集成测试用法）
- Modify: `templates/dcn/ARCHITECTURE.md`（验证体系一节补集成测试说明）
- Modify: `internal/template/templates.tar`（go generate 产物）

**Interfaces:**
- Consumes: Task 6 的测试包

- [ ] **Step 1: Makefile**

`templates/dcn/Makefile` 的 `.PHONY` 行加 `integration-test`，并加 target：

```make
integration-test:
	go test -tags=integration -p 1 ./test/integration/...
```

- [ ] **Step 2: CI**

`.github/workflows/ci.yml` 的 `dcn-e2e` job，在 `run: cd templates/dcn && make verify` 一步之后加：

```yaml
      - run: cd templates/dcn && make integration-test
```

- [ ] **Step 3: 文档**

README.md 与 README.zh-CN.md 的测试/开发相关段落（参照「Other targets」一行）补：`make integration-test`——对运行中的栈发起的外部集成测试（Go，build tag `integration`；栈不可达自动 Skip；可用 `DCN_IT_*` 环境变量覆盖端点）。ARCHITECTURE.md 的 verify 章节后补一段：集成测试包的定位（verify.sh 是编排式故障注入冒烟，integration 包是按服务的外部行为契约测试）。

- [ ] **Step 4: 全量验收（必须真跑）**

```bash
cd templates/dcn
docker compose --profile expansion down -v --remove-orphans
make up && make seed
go test ./...                       # 单测全绿（无栈依赖）
make integration-test               # 有栈全部 PASS，无 Skip
make verify && make topology-test   # 既有 8 关 + 拓扑不受影响
cd ../.. && go build ./... && go test ./...   # jiade 级回归
go generate ./internal/template     # 重新打包 tar
```

Expected: 全绿。集成测试若有个别用例失败，读日志定位（允许修复本计划 Task 1–6 引入的问题，每处在报告中说明根因）。

- [ ] **Step 5: Commit**

```bash
git add templates/dcn/Makefile templates/dcn/README.md templates/dcn/README.zh-CN.md templates/dcn/ARCHITECTURE.md .github/workflows/ci.yml internal/template/templates.tar
git commit -m "test(dcn): wire integration suite into make/ci/docs, re-embed tar"
```

---

## 任务依赖与执行顺序

Task 1（sqltest）→ Task 2–5（四个服务单测，均依赖 Task 1，彼此独立但按序执行）→ Task 6（集成测试包，依赖 Task 2/5 的 publishFn 仅间接——无代码依赖，可在 5 后执行）→ Task 7（接线 + 全量验收）。每个任务独立可测、独立提交。
