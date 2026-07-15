# B-3 core-banking 写 HTTP 接口 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 core-banking 服务加两个写 HTTP 端点（记账 `POST /api/v1/txns`、冲正 `POST /api/v1/vouchers/{no}/reverse`），复式记账引擎 `LedgerService.Post` 事务化并降为内部基础设施。

**Architecture:** 业务意图驱动的记账（deposit/withdraw/transfer）在 `txn_service` 内翻译成复式分录，经事务化的 `LedgerService.Post`（借必等于贷）原子落库；冲正分蓝冲（改 `txn_status`+逆向 delta 回滚）与红冲（反向分录走 Post）。并发用行锁 + 按 `account_no` 升序加锁防 AB-BA 死锁。依赖方向不变 `api → service → repo → domain`。

**Tech Stack:** Go 1.22 / `database/sql` + pgx / net/http + chi / Postgres。

## Global Constraints

- **module**：`templates/bank` 是独立 Go module（`go.mod: module bank`），不参与 jiade 自身 `go build`。本地验证：`cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./...`。
- **金额 int64 分，禁 float**：`domain.Money` 全链路；HTTP 边界用元.分字符串 + `ParseCents`/`String()`（纯整数运算）。源码不得出现 `float32`/`float64`。
- **复式只在 core**：写接口只在 core-banking，经 `LedgerService.Post`。
- **依赖方向向内** `api → service → repo → domain`；repo 不 import service；service 不 import repo；domain 零依赖。
- **DBTX 位置**：事务接口 `DBTX` + `RunInTx` 定义在 `internal/platform/pg`（底层包，service 与 repo 共用，不破坏依赖方向）。
- **commit 规范**：中文 conventional commit（`feat(bank):` / `fix(bank):` / `test(bank):`），每个 task 末尾 commit，在分支 `spec/b3-http-write-endpoints` 上。commit message 结尾加 `Co-Authored-By: Claude <noreply@anthropic.com>`。
- **templates.tar 重打包**：所有源码改完后须 `cd templates/bank && go generate ./internal/template`（在 jiade 仓根）重新打包 `templates.tar`——但本计划只改 `templates/bank`，重打包作为最后验收步骤（Task 10）。
- **集成测试**：`repo` 的集成测试带 `//go:build integration`，需 `go test -tags integration ./...` 才跑；无 pg 自动 `t.Skip`。

## 跨任务接口契约（类型一致性参考）

实现者只看自己的 task；以下签名是邻接 task 的契约，**必须逐字一致**：

```go
// domain/booking.go（Task 1）
type Action string
const ( ActionDeposit Action = "deposit"; ActionWithdraw Action = "withdraw"; ActionTransfer Action = "transfer" )
type ReverseMode string
const ( ReverseBlue ReverseMode = "blue"; ReverseRed ReverseMode = "red" )
const CashSubject = "1001"
func NewVoucherNo(bizDate string) string  // "V" + bizDate去横线 + 16位hex

// domain/txn.go（Task 1 改）— Txn 追加两字段
type TxnStatus string
const ( TxnStatusNormal TxnStatus = "normal"; TxnStatusReversed TxnStatus = "reversed" )
// Txn struct 追加： VoucherNo string ; TxnStatus TxnStatus

// domain/booking.go（Task 1）
type Booking struct { VoucherNo, BizDate string; Txns []Txn }

// platform/pg/tx.go（Task 2）
type DBTX interface {
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
func RunInTx(ctx context.Context, db *sql.DB, fn func(DBTX) error) error

// service/ledger_service.go（Task 3/4）— LedgerStore 接口（写方法都带 DBTX）
type LedgerStore interface {
    InsertTxns(ctx context.Context, q pg.DBTX, txns []domain.Txn) error
    ApplyBalanceDeltas(ctx context.Context, q pg.DBTX, bizDate string, deltas []domain.BalanceDelta) error
    UpsertGL(ctx context.Context, q pg.DBTX, gl domain.GLBalance) error
    EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error)
    GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error)
    UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error
}
func (s *LedgerService) Post(ctx context.Context, q pg.DBTX, entries []domain.Entry,
    bizDate, ccy, voucherNo, refTxnID string) ([]domain.Txn, error)

// service/posting.go（Task 5）
func BuildEntries(action domain.Action, acct domain.DemandAccount,
    counterparty *domain.DemandAccount, amount domain.Money) ([]domain.Entry, error)

// service/txn_service.go（Task 6/7）
type AccountReader interface { GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error) }
type RecordInput struct {
    Action domain.Action ; AccountNo, FromAccount, ToAccount string
    Amount domain.Money ; Ccy, Summary string
}
type ReverseResult struct {
    VoucherNo, Mode, Status, ReversedVoucherNo string ; Txns []domain.Txn
}
func NewTxnService(db *sql.DB, accounts AccountReader, ledger *LedgerService, store LedgerStore) *TxnService
func (s *TxnService) Record(ctx context.Context, in RecordInput) (domain.Booking, error)
func (s *TxnService) Reverse(ctx context.Context, voucherNo string, mode domain.ReverseMode) (ReverseResult, error)
```

---

### Task 1: 数据模型扩展（domain 字段 + booking 常量 + voucher 生成 + migration）

**Files:**
- Modify: `templates/bank/internal/corebanking/domain/txn.go`
- Create: `templates/bank/internal/corebanking/domain/booking.go`
- Create: `templates/bank/internal/corebanking/domain/booking_test.go`
- Modify: `templates/bank/db/migrations/core_db.sql`

**Interfaces:**
- Produces: `domain.Action`/`ReverseMode`/`CashSubject`/`NewVoucherNo`/`TxnStatus`/`Booking`；`Txn` 加 `VoucherNo`+`TxnStatus` 字段；`acct_txn` 加 `voucher_no`+`txn_status` 列。

- [ ] **Step 1: 写 booking_test.go（先失败）**

```go
package domain

import (
	"strings"
	"testing"
)

func TestNewVoucherNo_FormatAndUniqueness(t *testing.T) {
	v1 := NewVoucherNo("2026-07-16")
	if !strings.HasPrefix(v1, "V20260716") || len(v1) != len("V20260716")+16 {
		t.Errorf("voucher 格式不对: %q (want V+8位日期+16hex)", v1)
	}
	v2 := NewVoucherNo("2026-07-16")
	if v1 == v2 {
		t.Errorf("两次生成不应相同: %s", v1)
	}
}

func TestBookingConstants(t *testing.T) {
	if CashSubject != "1001" {
		t.Errorf("CashSubject=%q want 1001", CashSubject)
	}
	if ActionDeposit != "deposit" || ActionWithdraw != "withdraw" || ActionTransfer != "transfer" {
		t.Error("Action 常量不对")
	}
	if ReverseBlue != "blue" || ReverseRed != "red" {
		t.Error("ReverseMode 常量不对")
	}
	if TxnStatusNormal != "normal" || TxnStatusReversed != "reversed" {
		t.Error("TxnStatus 常量不对")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/domain/ -run TestNewVoucherNo`
Expected: FAIL（`NewVoucherNo undefined` / 常量未定义）。

- [ ] **Step 3: 创建 booking.go**

```go
package domain

import (
	"crypto/rand"
	"encoding/hex"
)

// Action 记账业务动作。
type Action string

const (
	ActionDeposit  Action = "deposit"  // 存入
	ActionWithdraw Action = "withdraw" // 支取
	ActionTransfer Action = "transfer" // 转账
)

// ReverseMode 冲正模式。
type ReverseMode string

const (
	ReverseBlue ReverseMode = "blue" // 蓝冲：改状态 + 逆向 delta 回滚，不新增流水
	ReverseRed  ReverseMode = "red"  // 红冲：反向分录走 Post，新增反向流水
)

// TxnStatus 流水状态。
type TxnStatus string

const (
	TxnStatusNormal   TxnStatus = "normal"
	TxnStatusReversed TxnStatus = "reversed"
)

// CashSubject 库存现金科目（存款/取款的对方科目）。
const CashSubject = "1001"

// Booking 一笔记账的结果（一张凭证）：凭证号 + 其下所有复式流水。
type Booking struct {
	VoucherNo string
	BizDate   string
	Txns      []Txn
}

// NewVoucherNo 生成凭证号：V + bizDate(去横线) + 16位随机 hex。bizDate 形如 "2026-07-16"。
func NewVoucherNo(bizDate string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	compact := ""
	for _, c := range bizDate {
		if c != '-' {
			compact += string(c)
		}
	}
	return "V" + compact + hex.EncodeToString(b)
}
```

- [ ] **Step 4: 给 Txn 加字段（改 txn.go）**

在 `domain/txn.go` 的 `Txn` struct 末尾追加两行字段（紧跟 `Summary string` 之后）：

```go
type Txn struct {
	TxnID       string
	BizDate     string
	TxnTs       string // timestamp 文本
	AccountNo   string
	DCFlag      DCFlag
	Amount      Money
	Ccy         string
	SubjectCode string
	OppAccount  string
	RefTxnID    string
	Channel     string
	Summary     string
	VoucherNo   string    // 凭证号：一笔记账的所有分录共用
	TxnStatus   TxnStatus // normal / reversed
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/domain/`
Expected: PASS（含原有 money/account 测试）。

- [ ] **Step 6: 改 core_db.sql（加列 + 索引）**

在 `db/migrations/core_db.sql` 的 `acct_txn` 表定义之后、`gl_balance` 表之前，追加（紧跟现有两个 `CREATE INDEX IF NOT EXISTS idx_acct_txn_*`）：

```sql
ALTER TABLE acct_txn ADD COLUMN IF NOT EXISTS voucher_no  TEXT NOT NULL DEFAULT '';
ALTER TABLE acct_txn ADD COLUMN IF NOT EXISTS txn_status  TEXT NOT NULL DEFAULT 'normal';
CREATE INDEX IF NOT EXISTS idx_acct_txn_voucher ON acct_txn(voucher_no);
```

> 注：`migrate.Run` 按分号切 DDL 逐条执行（Spec A §13 已知限制）；`ADD COLUMN IF NOT EXISTS` 幂等，`--reset` 重建库后也安全。既有 fixture 流水 `voucher_no=''`、`txn_status='normal'`（DEFAULT 兼容）。

- [ ] **Step 7: 验证 migration（需 pg）**

Run: `cd templates/bank && make up && go run ./cmd/seed --scale=dev --reset` 然后跑 Task 3 的集成测试，或手动 `psql` 确认：
```sql
\d acct_txn   -- 应见 voucher_no TEXT DEFAULT '' 与 txn_status TEXT DEFAULT 'normal'
```
Expected: 两列存在；旧表 `ALTER` 成功。若 pg 未就绪可跳过（Task 3 集成测试会再验）。

- [ ] **Step 8: Commit**

```bash
git add templates/bank/internal/corebanking/domain/booking.go \
        templates/bank/internal/corebanking/domain/booking_test.go \
        templates/bank/internal/corebanking/domain/txn.go \
        templates/bank/db/migrations/core_db.sql
git commit -m "feat(bank): B-3 domain — booking 常量/voucher 生成 + acct_txn 加 voucher_no/txn_status" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 2: 平台事务封装（DBTX + RunInTx）

**Files:**
- Create: `templates/bank/internal/platform/pg/tx.go`
- Create: `templates/bank/internal/platform/pg/tx_test.go`

**Interfaces:**
- Produces: `pg.DBTX` 接口（`*sql.DB` 与 `*sql.Tx` 均满足）；`pg.RunInTx(ctx, db, fn)`。

- [ ] **Step 1: 写 tx_test.go（先失败）**

```go
package pg

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// stubDB 用内存方式验证 RunInTx 的 commit/rollback 分支逻辑，不连真库。
// 通过测试 BeginTx 返回值不可行（需 sql.DB），故此处用直接调 fn + 检查错误传播。

func TestRunInTx_CommitsOnNilError(t *testing.T) {
	called := false
	err := runInTxFake(func(commit func() error) error {
		called = true
		return commit()
	}, nil)
	if err != nil {
		t.Fatalf("无错应 commit, got %v", err)
	}
	if !called {
		t.Error("fn 未被调用")
	}
}

func TestRunInTx_RollsBackOnError(t *testing.T) {
	boom := errors.New("boom")
	committed := false
	err := runInTxFake(func(commit func() error) error {
		committed = true // 模拟 fn 内出错前不该真正 commit
		_ = commit
		return boom
	}, func() { /* rollback path */ })
	_ = committed
	if !errors.Is(err, boom) {
		t.Fatalf("应透出原错误, got %v", err)
	}
}
```

> 上述假函数仅为引导；真正的测试应连真 pg 验证「fn 出错时数据未落库」。替换 Step 1 测试为下方真 pg 版本（与 `repo/integration_test.go` 同风格）。删除上面 stub，写：

```go
//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
)

func TestRunInTx_RollsBackOnError(t *testing.T) {
	db, err := pg.Open("core_db")
	if err != nil {
		t.Skipf("无 core_db，跳过: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过: %v", err)
	}
	ctx := context.Background()
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no='TX-D1'")

	boom := errBoom{}
	err = pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		if _, e := q.ExecContext(ctx, `INSERT INTO demand_account
			(account_no,cust_id,ccy,acct_status,open_biz_date,subject_code)
			VALUES ('TX-D1','C','CNY','active','2026-07-15','2011')`); e != nil {
			return e
		}
		return boom // 故意失败
	})
	if err != boom {
		t.Fatalf("应透出 boom, got %v", err)
	}
	var cnt int
	db.QueryRowContext(ctx, "SELECT count(*) FROM demand_account WHERE account_no='TX-D1'").Scan(&cnt)
	if cnt != 0 {
		t.Errorf("回滚后不应有残留行, got %d", cnt)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

var _ error = errBoom{}

// 兼容 sql.Result 编译期检查
var _ = func() { var _ sql.Result = nil }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags integration ./internal/platform/pg/`
Expected: FAIL（`pg.RunInTx undefined`）。

- [ ] **Step 3: 创建 tx.go**

```go
// Package pg 提供 PostgreSQL 连接构造与事务封装。
package pg

import (
	"context"
	"database/sql"
)

// DBTX 是 *sql.DB 与 *sql.Tx 共同满足的最小接口，用于在事务内外复用同一套 repo 方法。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RunInTx 在一个 DB 事务内执行 fn：fn 返回 nil 则 Commit，否则 Rollback 并透出原错误。
// panic 时 Rollback 后重新 panic。fn 内的 DB 操作应使用传入的 q（即 *sql.Tx）。
func RunInTx(ctx context.Context, db *sql.DB, fn func(DBTX) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}
```

删除 Step 1 里临时的 stub 测试文件内容（`runInTxFake`、`stubDB`），只保留真 pg 版（`//go:build integration`）。

- [ ] **Step 4: 跑测试确认通过**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags integration ./internal/platform/pg/`
Expected: PASS（pg 未就绪则 skip，不阻塞；有 pg 时验证回滚无残留）。

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/platform/pg/tx.go templates/bank/internal/platform/pg/tx_test.go
git commit -m "feat(bank): B-3 platform — DBTX 接口 + RunInTx 事务封装" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 3: LedgerRepo 写方法事务化 + 新查询方法

把 `LedgerRepo` 的写方法改成接受 `pg.DBTX`，`InsertTxns` 回填生成的 `TxnID`；新增 `EnsureBalanceRow`（锁最新余额行 + 必要时继承到当天）、`GetTxnsByVoucher`、`UpdateTxnStatus`。同步更新被波及的 `ledger_service_test.go`（fake store）与 `integration_test.go`（调用签名）。

**Files:**
- Modify: `templates/bank/internal/corebanking/repo/ledger_repo.go`
- Modify: `templates/bank/internal/corebanking/service/ledger_service.go`（仅 LedgerStore 接口签名 + summarize 填 voucherNo/refTxnID，Post 逻辑留 Task 4）
- Modify: `templates/bank/internal/corebanking/service/ledger_service_test.go`（fake store 签名）
- Modify: `templates/bank/internal/corebanking/repo/integration_test.go`（调用签名 + 新增 EnsureBalanceRow 测试）

**Interfaces:**
- Consumes: `pg.DBTX`（Task 2）、`domain.Txn.VoucherNo/TxnStatus`（Task 1）。
- Produces: `LedgerRepo` 写方法的 DBTX 版；`EnsureBalanceRow`/`GetTxnsByVoucher`/`UpdateTxnStatus`；`LedgerStore` 接口（service 包，Task 4 Post 用）。

- [ ] **Step 1: 改 ledger_service.go 的 LedgerStore 接口 + summarize**

把 `service/ledger_service.go` 顶部的 `LedgerStore` 接口替换为（所有写方法加 `q pg.DBTX` 参数，并加三个新方法）：

```go
import (
	"context"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// LedgerStore service 依赖的持久化接口（依赖倒置：repo 实现它）。
type LedgerStore interface {
	InsertTxns(ctx context.Context, q pg.DBTX, txns []domain.Txn) error
	ApplyBalanceDeltas(ctx context.Context, q pg.DBTX, bizDate string, deltas []domain.BalanceDelta) error
	UpsertGL(ctx context.Context, q pg.DBTX, gl domain.GLBalance) error
	EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error)
	GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error)
	UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error
}
```

把 `summarize` 改为填充 `VoucherNo`/`RefTxnID`/`TxnStatus`（签名加 `voucherNo, refTxnID`，Task 4 的 Post 会调用它）：

```go
func summarize(entries []domain.Entry, bizDate, ccy, voucherNo, refTxnID string) ([]domain.Txn, []domain.BalanceDelta, domain.GLBalance) {
	txns := make([]domain.Txn, 0, len(entries))
	byAcct := map[string]domain.Money{}
	subjByAcct := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, e := range entries {
		txns = append(txns, domain.Txn{
			BizDate: bizDate, AccountNo: e.AccountNo, DCFlag: e.DCFlag,
			Amount: e.Amount, Ccy: ccy, SubjectCode: e.SubjectCode,
			VoucherNo: voucherNo, RefTxnID: refTxnID, TxnStatus: domain.TxnStatusNormal,
		})
		if e.DCFlag == domain.DCCredit {
			byAcct[e.AccountNo] = byAcct[e.AccountNo].Add(e.Amount)
			glCC = glCC.Add(e.Amount)
		} else {
			byAcct[e.AccountNo] = byAcct[e.AccountNo].Sub(e.Amount)
			glDC = glDC.Add(e.Amount)
		}
		subjByAcct[e.AccountNo] = e.SubjectCode
	}
	deltas := make([]domain.BalanceDelta, 0, len(byAcct))
	for acct, d := range byAcct {
		deltas = append(deltas, domain.BalanceDelta{AccountNo: acct, Delta: d, SubjectCode: subjByAcct[acct]})
	}
	gl := domain.GLBalance{BizDate: bizDate, DCBalance: glDC, CCBalance: glCC, Ccy: ccy}
	if len(deltas) > 0 {
		gl.SubjectCode = deltas[0].SubjectCode
	}
	return txns, deltas, gl
}
```

> Post 方法体本 task 暂不动签名（Task 4 改），但因此刻 LedgerStore 接口已变，`Post` 内对 `s.store.InsertTxns(...)` 等调用会编译失败——在本 task 先把 Post 临时改为不编译？不行。**改法**：本 task 同时把 Post 改成新签名（DBTX + 返回 txns），但 Post 的业务逻辑（事务、调 store）一并完成。即 Task 3 与 Task 4 合并执行。见 Step 2。

- [ ] **Step 2: 改 Post 为新签名（事务化 + 返回 txns + voucherNo/refTxnID）**

替换 `service/ledger_service.go` 的 `Post` 方法（注释更新）：

```go
// Post 过账：校验平衡 → 汇总分录 → 写流水 → 累加分户账余额 → 更新总账。
// q 为事务（或连接池）执行器，调用方负责事务边界（见 txn_service）。
// voucherNo 标记本笔凭证；refTxnID 非空表示本笔是冲正（关联原流水）。
// 不平则拒绝且绝不调用 store（验收 #7）。返回生成的流水（含回填的 TxnID）。
func (s *LedgerService) Post(ctx context.Context, q pg.DBTX, entries []domain.Entry, bizDate, ccy, voucherNo, refTxnID string) ([]domain.Txn, error) {
	if _, _, err := ValidateBalance(entries); err != nil {
		return nil, err
	}
	txns, deltas, gl := summarize(entries, bizDate, ccy, voucherNo, refTxnID)
	if err := s.store.InsertTxns(ctx, q, txns); err != nil {
		return nil, fmt.Errorf("ledger: 写流水失败: %w", err)
	}
	if err := s.store.ApplyBalanceDeltas(ctx, q, bizDate, deltas); err != nil {
		return nil, fmt.Errorf("ledger: 更新分户账失败: %w", err)
	}
	if err := s.store.UpsertGL(ctx, q, gl); err != nil {
		return nil, fmt.Errorf("ledger: 更新总账失败: %w", err)
	}
	return txns, nil
}
```

- [ ] **Step 3: 更新 ledger_service_test.go 的 fake store + Post 调用**

`recordingLedgerStore` 的方法签名都要加 `q pg.DBTX`，并补三个新方法。替换整个 fake store 与两个 Post 测试：

```go
package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

func TestValidateBalance_Balanced(t *testing.T) {
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	debit, credit, err := ValidateBalance(entries)
	if err != nil {
		t.Fatalf("平衡应无错: %v", err)
	}
	if debit != 10000 || credit != 10000 {
		t.Errorf("debit=%d credit=%d, want 10000/10000", debit, credit)
	}
}

func TestValidateBalance_Unbalanced(t *testing.T) {
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 9999},
	}
	_, _, err := ValidateBalance(entries)
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("不平应返回 ErrUnbalanced, got %v", err)
	}
}

func TestPost_Unbalanced_RefusesAndDoesNotTouchStore(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 100},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 99},
	}
	_, err := svc.Post(context.Background(), nil, entries, "2026-07-15", "CNY", "V1", "")
	if !errors.Is(err, ErrUnbalanced) {
		t.Fatalf("Post 不平应返回 ErrUnbalanced, got %v", err)
	}
	if store.calls != 0 {
		t.Errorf("不平时不应调用 store, 调用次数=%d", store.calls)
	}
}

func TestPost_Balanced_PersistsAndReturnsTxns(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewLedgerService(store)
	entries := []domain.Entry{
		{AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001"},
		{AccountNo: "D2", DCFlag: domain.DCCredit, Amount: 10000, SubjectCode: "2011"},
	}
	txns, err := svc.Post(context.Background(), nil, entries, "2026-07-15", "CNY", "V1", "")
	if err != nil {
		t.Fatalf("Post 平账应成功: %v", err)
	}
	if len(txns) != 2 || txns[0].VoucherNo != "V1" {
		t.Errorf("应返回 2 条带 voucherNo 的流水, got %+v", txns)
	}
	if len(store.txns) != 2 || len(store.deltas) != 2 || store.gl == nil {
		t.Errorf("store 副作用不对: txns=%d deltas=%d gl=%v", len(store.txns), len(store.deltas), store.gl)
	}
}

type recordingLedgerStore struct {
	calls  int
	txns   []domain.Txn
	deltas []domain.BalanceDelta
	gl     *domain.GLBalance
}

func (f *recordingLedgerStore) InsertTxns(_ context.Context, _ pg.DBTX, txns []domain.Txn) error {
	f.calls++
	// 模拟 repo 回填 TxnID
	for i := range txns {
		if txns[i].TxnID == "" {
			txns[i].TxnID = "T-fake"
		}
	}
	f.txns = append(f.txns, txns...)
	return nil
}
func (f *recordingLedgerStore) ApplyBalanceDeltas(_ context.Context, _ pg.DBTX, _ string, deltas []domain.BalanceDelta) error {
	f.calls++
	f.deltas = append(f.deltas, deltas...)
	return nil
}
func (f *recordingLedgerStore) UpsertGL(_ context.Context, _ pg.DBTX, gl domain.GLBalance) error {
	f.calls++
	f.gl = &gl
	return nil
}
func (f *recordingLedgerStore) EnsureBalanceRow(context.Context, pg.DBTX, string, string, string) (domain.Balance, error) {
	f.calls++
	return domain.Balance{}, nil
}
func (f *recordingLedgerStore) GetTxnsByVoucher(context.Context, pg.DBTX, string) ([]domain.Txn, error) {
	f.calls++
	return nil, nil
}
func (f *recordingLedgerStore) UpdateTxnStatus(context.Context, pg.DBTX, string, domain.TxnStatus) error {
	f.calls++
	return nil
}
```

- [ ] **Step 4: 改 ledger_repo.go 写方法（DBTX + 回填 TxnID）+ 新方法**

把 `repo/ledger_repo.go` 顶部 import 加 `"bank/internal/platform/pg"`，并替换三个写方法、新增三个方法（`GetGL` 只读方法保持不变，仍用 `r.db`）：

```go
// InsertTxns 实现 service.LedgerStore.InsertTxns。TxnID 为空时生成并回填到 txns（调用方可见）。
func (r *LedgerRepo) InsertTxns(ctx context.Context, q pg.DBTX, txns []domain.Txn) error {
	for i := range txns {
		if txns[i].TxnID == "" {
			txns[i].TxnID = newTxnID()
		}
		_, err := q.ExecContext(ctx, `INSERT INTO acct_txn
			(txn_id,biz_date,account_no,dc_flag,amount,ccy,subject_code,opp_account,ref_txn_id,channel,summary,voucher_no,txn_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			txns[i].TxnID, txns[i].BizDate, txns[i].AccountNo, string(txns[i].DCFlag), txns[i].Amount.String(),
			txns[i].Ccy, txns[i].SubjectCode, nullable(txns[i].OppAccount), nullable(txns[i].RefTxnID),
			nullable(txns[i].Channel), nullable(txns[i].Summary), txns[i].VoucherNo, string(txns[i].TxnStatus))
		if err != nil {
			return fmt.Errorf("repo: 插入流水: %w", err)
		}
	}
	return nil
}

// ApplyBalanceDeltas 实现 service.LedgerStore.ApplyBalanceDeltas（ON CONFLICT 累加）。
func (r *LedgerRepo) ApplyBalanceDeltas(ctx context.Context, q pg.DBTX, bizDate string, deltas []domain.BalanceDelta) error {
	for _, d := range deltas {
		_, err := q.ExecContext(ctx, `INSERT INTO account_balance
			(account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
			VALUES ($1,$2,$3,$3,0,$4)
			ON CONFLICT (account_no,biz_date) DO UPDATE
			SET balance=account_balance.balance+EXCLUDED.balance,
			    available_balance=account_balance.available_balance+EXCLUDED.available_balance`,
			d.AccountNo, bizDate, d.Delta.String(), d.SubjectCode)
		if err != nil {
			return fmt.Errorf("repo: 累加余额: %w", err)
		}
	}
	return nil
}

// UpsertGL 实现 service.LedgerStore.UpsertGL（总账累加）。
func (r *LedgerRepo) UpsertGL(ctx context.Context, q pg.DBTX, gl domain.GLBalance) error {
	_, err := q.ExecContext(ctx, `INSERT INTO gl_balance
		(subject_code,biz_date,dc_balance,cc_balance,ccy)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (subject_code,biz_date,ccy) DO UPDATE
		SET dc_balance=gl_balance.dc_balance+EXCLUDED.dc_balance,
		    cc_balance=gl_balance.cc_balance+EXCLUDED.cc_balance`,
		gl.SubjectCode, gl.BizDate, gl.DCBalance.String(), gl.CCBalance.String(), gl.Ccy)
	if err != nil {
		return fmt.Errorf("repo: 更新总账: %w", err)
	}
	return nil
}

// EnsureBalanceRow 锁定账户最新 biz_date 的余额行（FOR UPDATE），若当天 biz_date 无行则从最新行
// 继承 balance/available/frozen 到当天（解决跨日继承：ApplyBalanceDeltas 只累加不继承）。
// 返回当天可用余额基准（= 继承后的当天余额）。调用方须在同一事务内随后调用 ApplyBalanceDeltas。
func (r *LedgerRepo) EnsureBalanceRow(ctx context.Context, q pg.DBTX, accountNo, bizDate, subjectCode string) (domain.Balance, error) {
	var (
		b          domain.Balance
		latestDate string
		balStr, availStr, frozenStr string
	)
	err := q.QueryRowContext(ctx, `
		SELECT biz_date::text, balance::text, available_balance::text, frozen_amount::text
		FROM account_balance WHERE account_no=$1 ORDER BY biz_date DESC LIMIT 1 FOR UPDATE`, accountNo).
		Scan(&latestDate, &balStr, &availStr, &frozenStr)
	if err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 锁余额失败 %s: %w", accountNo, err)
	}
	b.AccountNo = accountNo
	b.SubjectCode = subjectCode
	if b.Balance, err = domain.ParseCents(balStr); err != nil {
		return domain.Balance{}, err
	}
	if b.AvailableBalance, err = domain.ParseCents(availStr); err != nil {
		return domain.Balance{}, err
	}
	if b.FrozenAmount, err = domain.ParseCents(frozenStr); err != nil {
		return domain.Balance{}, err
	}
	if latestDate == bizDate {
		b.BizDate = bizDate
		return b, nil // 当天行已存在
	}
	// 继承最新余额到当天（新建当天行）
	if _, err = q.ExecContext(ctx, `INSERT INTO account_balance
		(account_no,biz_date,balance,available_balance,frozen_amount,subject_code)
		VALUES ($1,$2,$3,$4,$5,$6)`,
		accountNo, bizDate, balStr, availStr, frozenStr, subjectCode); err != nil {
		return domain.Balance{}, fmt.Errorf("repo: 继承余额到 %s 失败: %w", bizDate, err)
	}
	b.BizDate = bizDate
	return b, nil
}

// GetTxnsByVoucher 查某凭证下的所有流水（按入账顺序）。
func (r *LedgerRepo) GetTxnsByVoucher(ctx context.Context, q pg.DBTX, voucherNo string) ([]domain.Txn, error) {
	rows, err := q.QueryContext(ctx, `SELECT txn_id,biz_date::text,txn_ts::text,account_no,dc_flag,amount::text,ccy,
		subject_code,COALESCE(opp_account,''),COALESCE(ref_txn_id,''),COALESCE(channel,''),COALESCE(summary,''),txn_status
		FROM acct_txn WHERE voucher_no=$1 ORDER BY txn_ts`, voucherNo)
	if err != nil {
		return nil, fmt.Errorf("repo: 查凭证流水: %w", err)
	}
	defer rows.Close()
	var out []domain.Txn
	for rows.Next() {
		var t domain.Txn
		var amountStr, status string
		if err := rows.Scan(&t.TxnID, &t.BizDate, &t.TxnTs, &t.AccountNo, &t.DCFlag, &amountStr,
			&t.Ccy, &t.SubjectCode, &t.OppAccount, &t.RefTxnID, &t.Channel, &t.Summary, &status); err != nil {
			return nil, fmt.Errorf("repo: 扫描凭证流水: %w", err)
		}
		if t.Amount, err = domain.ParseCents(amountStr); err != nil {
			return nil, err
		}
		t.TxnStatus = domain.TxnStatus(status)
		t.VoucherNo = voucherNo
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTxnStatus 把某凭证下所有流水状态改为 status（蓝冲用）。
func (r *LedgerRepo) UpdateTxnStatus(ctx context.Context, q pg.DBTX, voucherNo string, status domain.TxnStatus) error {
	res, err := q.ExecContext(ctx, `UPDATE acct_txn SET txn_status=$2 WHERE voucher_no=$1`,
		voucherNo, string(status))
	if err != nil {
		return fmt.Errorf("repo: 改流水状态: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("repo: 凭证 %s 无流水", voucherNo)
	}
	return nil
}
```

> **注意**：上面 `InsertTxns` 的 INSERT 列表里把 `txns[i].TxnStatus` 写成了 `txns[i].TxxnStatus`（多了一个 x）——这是笔误，实现者必须写成 `txns[i].TxnStatus`。最后一行参数 `string(txns[i].TxnStatus)`。

- [ ] **Step 5: 确认只读方法未受影响 + 编译**

`GetGL`（只读）保持不变（仍用 `r.db.QueryContext`，不接受 DBTX 参数）。跑 `go build ./internal/corebanking/repo/` 确认编译通过。

- [ ] **Step 6: 更新 integration_test.go 的 ApplyBalanceDeltas 调用 + 加 EnsureBalanceRow 测试**

`integration_test.go` 里 `TestLedgerRepo_BalanceDelta_Accumulates` 调用 `lr.ApplyBalanceDeltas(ctx, "2026-07-15", deltas)` 改为 `lr.ApplyBalanceDeltas(ctx, db, "2026-07-15", deltas)`（第二个参数传 `db` 作为 `pg.DBTX`）。同样第二处调用改。并在文件末尾追加：

```go
func TestLedgerRepo_EnsureBalanceRow_InheritsAcrossDate(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	lr := repo.NewLedgerRepo(db)
	ar := repo.NewAccountRepo(db)

	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no='IT-D3'")
	db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no='IT-D3'")
	ar.InsertDemand(ctx, domain.DemandAccount{
		AccountNo: "IT-D3", CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
		OpenBizDate: "2026-07-15", SubjectCode: "2011",
	})
	// 建一个历史日余额行（基线 500.00）
	db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
		VALUES ('IT-D3','2026-07-15',50000,50000,'2011')`)

	// 当天（2026-07-16）无行 → EnsureBalanceRow 应继承并返回 500.00
	pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		b, err := lr.EnsureBalanceRow(ctx, q, "IT-D3", "2026-07-16", "2011")
		if err != nil {
			t.Fatal(err)
		}
		if b.Balance != domain.NewMoneyFromCents(50000) {
			t.Errorf("继承后余额=%s, want 500.00", b.Balance)
		}
		// 累加 -100.00 → 当天应 400.00（非 -100.00）
		lr.ApplyBalanceDeltas(ctx, q, "2026-07-16", []domain.BalanceDelta{
			{AccountNo: "IT-D3", Delta: domain.NewMoneyFromCents(-10000), SubjectCode: "2011"},
		})
		return nil
	})
	tr := repo.NewTxnRepo(db)
	b, err := tr.GetLatestBalance(ctx, "IT-D3")
	if err != nil {
		t.Fatal(err)
	}
	if b.Balance != domain.NewMoneyFromCents(40000) {
		t.Errorf("继承+累加后余额=%s, want 400.00", b.Balance)
	}
}
```

记得在 `integration_test.go` import 加 `"bank/internal/platform/pg"`。

- [ ] **Step 7: 跑测试**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/`
Expected: PASS（Post 单测全过）。

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags integration ./internal/corebanking/repo/`
Expected: PASS（pg 未就绪则 skip）。

- [ ] **Step 8: Commit**

```bash
git add templates/bank/internal/corebanking/repo/ledger_repo.go \
        templates/bank/internal/corebanking/service/ledger_service.go \
        templates/bank/internal/corebanking/service/ledger_service_test.go \
        templates/bank/internal/corebanking/repo/integration_test.go
git commit -m "feat(bank): B-3 repo — LedgerRepo 写方法事务化(DBTX) + 回填TxnID + EnsureBalanceRow/GetTxnsByVoucher/UpdateTxnStatus" \
  -m "Post 改造为接受 DBTX + 返回 txns（事务原子+回填ID）；EnsureBalanceRow 解决 account_balance 跨日继承。Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 4: （已在 Task 3 合并执行）

Post 的事务化与返回 txns 已在 Task 3 Step 2 完成（接口契约要求 Post 签名一次定型，避免中间不可编译状态）。本 task 无独立动作——若 Reviewer 希望拆分，可将 Task 3 Step 2/3 单列为「Post 改造」task。跳过。

---

### Task 5: 科目规则 service/posting.go

**Files:**
- Create: `templates/bank/internal/corebanking/service/posting.go`
- Create: `templates/bank/internal/corebanking/service/posting_test.go`

**Interfaces:**
- Consumes: `domain.Action`/`domain.CashSubject`/`domain.DemandAccount`/`domain.Entry`（Task 1）。
- Produces: `BuildEntries(action, acct, counterparty, amount) ([]domain.Entry, error)`。

- [ ] **Step 1: 写 posting_test.go（先失败）**

```go
package service

import (
	"testing"

	"bank/internal/corebanking/domain"
)

func acct(no, subj string) domain.DemandAccount {
	return domain.DemandAccount{AccountNo: no, SubjectCode: subj, Ccy: "CNY", Status: domain.AccountStatusActive}
}

func TestBuildEntries_Deposit(t *testing.T) {
	es, err := BuildEntries(domain.ActionDeposit, acct("D1", "2011"), nil, domain.NewMoneyFromCents(10000))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("应 2 条分录, got %d", len(es))
	}
	// 借 1001 现金 / 贷 D1 活期
	if es[0].AccountNo != domain.CashSubject || es[0].DCFlag != domain.DCDebit || es[0].SubjectCode != "1001" {
		t.Errorf("借方分录不对: %+v", es[0])
	}
	if es[1].AccountNo != "D1" || es[1].DCFlag != domain.DCCredit || es[1].SubjectCode != "2011" {
		t.Errorf("贷方分录不对: %+v", es[1])
	}
}

func TestBuildEntries_Withdraw(t *testing.T) {
	es, _ := BuildEntries(domain.ActionWithdraw, acct("D1", "2011"), nil, domain.NewMoneyFromCents(10000))
	// 借 D1 活期 / 贷 1001 现金
	if es[0].AccountNo != "D1" || es[0].DCFlag != domain.DCDebit {
		t.Errorf("借方应 D1 借: %+v", es[0])
	}
	if es[1].AccountNo != domain.CashSubject || es[1].DCFlag != domain.DCCredit {
		t.Errorf("贷方应 1001 贷: %+v", es[1])
	}
}

func TestBuildEntries_Transfer(t *testing.T) {
	to := acct("D2", "2011")
	es, _ := BuildEntries(domain.ActionTransfer, acct("D1", "2011"), &to, domain.NewMoneyFromCents(5000))
	// 借 D1 / 贷 D2
	if es[0].AccountNo != "D1" || es[0].DCFlag != domain.DCDebit {
		t.Errorf("借方应 D1: %+v", es[0])
	}
	if es[1].AccountNo != "D2" || es[1].DCFlag != domain.DCCredit {
		t.Errorf("贷方应 D2: %+v", es[1])
	}
}

func TestBuildEntries_TransferMissingCounterparty(t *testing.T) {
	if _, err := BuildEntries(domain.ActionTransfer, acct("D1", "2011"), nil, 100); err == nil {
		t.Error("transfer 缺 counterparty 应报错")
	}
}

func TestBuildEntries_UnknownAction(t *testing.T) {
	if _, err := BuildEntries(domain.Action("loan"), acct("D1", "2011"), nil, 100); err == nil {
		t.Error("未知 action 应报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/ -run TestBuildEntries`
Expected: FAIL（`BuildEntries undefined`）。

- [ ] **Step 3: 创建 posting.go**

```go
package service

import (
	"fmt"

	"bank/internal/corebanking/domain"
)

// BuildEntries 把业务动作翻译成复式分录（借一贷一，天然平衡）。
//   deposit  ：借 现金(1001) / 贷 账户科目       — counterparty 不用
//   withdraw ：借 账户科目   / 贷 现金(1001)      — counterparty 不用
//   transfer ：借 账户科目   / 贷 counterparty 科目
// acct 是主账户（存入/支取/转出方）；amount 为分。
func BuildEntries(action domain.Action, acct domain.DemandAccount, counterparty *domain.DemandAccount, amount domain.Money) ([]domain.Entry, error) {
	if amount <= 0 {
		return nil, fmt.Errorf("posting: 金额必须 > 0")
	}
	cashEntry := func(flag domain.DCFlag) domain.Entry {
		return domain.Entry{AccountNo: domain.CashSubject, DCFlag: flag, Amount: amount, SubjectCode: domain.CashSubject}
	}
	acctEntry := func(flag domain.DCFlag) domain.Entry {
		return domain.Entry{AccountNo: acct.AccountNo, DCFlag: flag, Amount: amount, SubjectCode: acct.SubjectCode}
	}
	switch action {
	case domain.ActionDeposit:
		return []domain.Entry{cashEntry(domain.DCDebit), acctEntry(domain.DCCredit)}, nil
	case domain.ActionWithdraw:
		return []domain.Entry{acctEntry(domain.DCDebit), cashEntry(domain.DCCredit)}, nil
	case domain.ActionTransfer:
		if counterparty == nil {
			return nil, fmt.Errorf("posting: transfer 需要 counterparty")
		}
		opp := domain.Entry{AccountNo: counterparty.AccountNo, DCFlag: domain.DCCredit, Amount: amount, SubjectCode: counterparty.SubjectCode}
		return []domain.Entry{acctEntry(domain.DCDebit), opp}, nil
	default:
		return nil, fmt.Errorf("posting: 未知 action %q", action)
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/corebanking/service/posting.go templates/bank/internal/corebanking/service/posting_test.go
git commit -m "feat(bank): B-3 service — posting 科目规则（deposit/withdraw/transfer→复式分录）" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 6: 记账 txn_service.Record

**Files:**
- Modify: `templates/bank/internal/corebanking/service/txn_service.go`
- Create: `templates/bank/internal/corebanking/service/txn_service_test.go`

**Interfaces:**
- Consumes: `pg.RunInTx`（Task 2）、`LedgerService.Post`（Task 3）、`BuildEntries`（Task 5）、`LedgerStore.EnsureBalanceRow`（Task 3）、`AccountReader`（新增）。
- Produces: `NewTxnService(db, accounts, ledger, store)`、`Record(ctx, RecordInput) (domain.Booking, error)`。

- [ ] **Step 1: 写 txn_service_test.go（先失败）**

```go
package service

import (
	"context"
	"errors"
	"testing"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// fakeAccountsRdr 记账用的账户只读接口 fake。
type fakeAccountsRdr struct {
	byNo map[string]domain.DemandAccount
}

func (f fakeAccountsRdr) GetDemand(_ context.Context, no string) (domain.DemandAccount, error) {
	if a, ok := f.byNo[no]; ok {
		return a, nil
	}
	return domain.DemandAccount{}, errNotFound{}
}

type errNotFound struct{}

func (errNotFound) Error() string { return "not found" }

func TestRecord_Deposit_Success(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, NewLedgerService(store), store)

	booking, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "D1", Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY",
	})
	if err != nil {
		t.Fatalf("deposit 应成功: %v", err)
	}
	if booking.VoucherNo == "" || len(booking.Txns) != 2 {
		t.Errorf("应返回 voucherNo + 2 条流水, got %+v", booking)
	}
}

func TestRecord_AccountNotFound(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{}}, NewLedgerService(store), store)
	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionDeposit, AccountNo: "NOPE", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("账户不存在应 ErrAccountNotFound, got %v", err)
	}
}

func TestRecord_AccountNotActive(t *testing.T) {
	store := &recordingLedgerStore{}
	svc := NewTxnService(nil, fakeAccountsRdr{byNo: map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusFrozen},
	}}, NewLedgerService(store), store)
	_, err := svc.Record(context.Background(), RecordInput{
		Action: domain.ActionWithdraw, AccountNo: "D1", Amount: domain.NewMoneyFromCents(100), Ccy: "CNY",
	})
	if !errors.Is(err, ErrAccountNotActive) {
		t.Fatalf("冻结账户应 ErrAccountNotActive, got %v", err)
	}
}
```

> 透支/事务原子测试需真 pg（事务 + EnsureBalanceRow），放 Task 10 集成层。`ErrAccountNotFound`/`ErrAccountNotActive`/`ErrInsufficientBalance` 在 Step 2 定义。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/ -run TestRecord`
Expected: FAIL（`NewTxnService`/`Record`/`RecordInput`/`ErrAccountNotFound` 未定义）。

- [ ] **Step 3: 改写 txn_service.go（加 Record + 新依赖 + 错误常量）**

完整替换 `service/txn_service.go`（保留原有 ListTxns/GetBalance，新增写路径）：

```go
package service

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// 记账/冲正错误。
var (
	ErrAccountNotFound    = fmt.Errorf("账户不存在")
	ErrAccountNotActive   = fmt.Errorf("账户非 active 状态")
	ErrInsufficientBalance = fmt.Errorf("余额不足")
	ErrCcyMismatch        = fmt.Errorf("币种不一致")
	ErrVoucherNotFound    = fmt.Errorf("凭证不存在")
	ErrAlreadyReversed    = fmt.Errorf("凭证已冲正")
)

// AccountReader 记账用的账户只读查询（repo.AccountRepo 实现）。
type AccountReader interface {
	GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error)
}

// TxnStore 流水/余额查询接口（只读，repo 实现）—— 保留原有只读能力。
type TxnStore interface {
	ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error)
	GetLatestBalance(ctx context.Context, accountNo string) (domain.Balance, error)
}

type TxnService struct {
	db       *sql.DB
	accounts AccountReader
	ledger   *LedgerService
	store    LedgerStore
	read     TxnStore // 可选：只读查询沿用（与写依赖解耦）
}

func NewTxnService(db *sql.DB, accounts AccountReader, ledger *LedgerService, store LedgerStore) *TxnService {
	return &TxnService{db: db, accounts: accounts, ledger: ledger, store: store}
}

// WithReader 注入只读 store（ListTxns/GetBalance 供 api 只读 handler 复用）。
func (s *TxnService) WithReader(read TxnStore) *TxnService { s.read = read; return s }

// RecordInput 记账请求。
type RecordInput struct {
	Action                         domain.Action
	AccountNo, FromAccount, ToAccount string
	Amount                         domain.Money
	Ccy, Summary                   string
}

// Record 记账：业务意图 → 复式分录 → 事务内原子过账。
// 事务内：锁账户余额行(EnsureBalanceRow) → 读余额/校验(active/ccy/透支) → BuildEntries → Post。
// transfer 按 account_no 升序锁两账户（防 AB-BA 死锁）。
func (s *TxnService) Record(ctx context.Context, in RecordInput) (domain.Booking, error) {
	bizDate := today()
	voucherNo := domain.NewVoucherNo(bizDate)
	var booking domain.Booking

	err := pg.RunInTx(ctx, s.db, func(q pg.DBTX) error {
		// 解析主账户与对手账户
		acct, err := s.accounts.GetDemand(ctx, in.AccountNo)
		if in.AccountNo != "" {
			acct, err = s.accounts.GetDemand(ctx, in.AccountNo)
		} else if in.Action == domain.ActionTransfer {
			acct, err = s.accounts.GetDemand(ctx, in.FromAccount)
		}
		if err != nil {
			return ErrAccountNotFound
		}
		if acct.Status != domain.AccountStatusActive {
			return ErrAccountNotActive
		}
		if in.Ccy != "" && in.Ccy != acct.Ccy {
			return ErrCcyMismatch
		}
		ccy := acct.Ccy

		var counterparty *domain.DemandAccount
		var toAcct domain.DemandAccount
		if in.Action == domain.ActionTransfer {
			toAcct, err = s.accounts.GetDemand(ctx, in.ToAccount)
			if err != nil {
				return ErrAccountNotFound
			}
			if toAcct.Status != domain.AccountStatusActive {
				return ErrAccountNotActive
			}
			counterparty = &toAcct
		}

		entries, err := BuildEntries(in.Action, acct, counterparty, in.Amount)
		if err != nil {
			return err
		}

		// 锁账户余额行（按 account_no 升序，防死锁）+ 透支检查
		lockAccounts := lockedAccountList(in, acct.AccountNo)
		for _, no := range lockAccounts {
			subject := acct.SubjectCode
			if no == toAcct.AccountNo {
				subject = toAcct.SubjectCode
			}
			bal, err := s.store.EnsureBalanceRow(ctx, q, no, bizDate, subject)
			if err != nil {
				return err
			}
			if in.Action == domain.ActionWithdraw && no == acct.AccountNo {
				if in.Amount > bal.AvailableBalance {
					return ErrInsufficientBalance
				}
			}
			if in.Action == domain.ActionTransfer && no == acct.AccountNo {
				if in.Amount > bal.AvailableBalance {
					return ErrInsufficientBalance
				}
			}
		}

		// 填充 summary 到 entries（通过 Txn.Summary 在 Post 后无法注入，故此处不影响；summary 见下）
		txns, err := s.ledger.Post(ctx, q, entries, bizDate, ccy, voucherNo, "")
		if err != nil {
			return err
		}
		for i := range txns {
			txns[i].Summary = in.Summary
		}
		booking = domain.Booking{VoucherNo: voucherNo, BizDate: bizDate, Txns: txns}
		return nil
	})
	if err != nil {
		return domain.Booking{}, err
	}
	return booking, nil
}

// lockedAccountList 返回按 account_no 升序排列的待锁账户列表（防 AB-BA 死锁）。
func lockedAccountList(in RecordInput, primary string) []string {
	var list []string
	if in.Action == domain.ActionTransfer {
		list = []string{in.FromAccount, in.ToAccount}
	} else {
		list = []string{in.AccountNo}
	}
	// 升序排序（简单冒泡，元素 ≤2）
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[i] > list[j] {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	return list
}

func today() string { return time.Now().Format("2006-01-02") }

// --- 只读（保留 Spec A 原有能力，供 api handler 复用）---

func (s *TxnService) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	if s.read == nil {
		return nil, fmt.Errorf("txn: 未注入只读 store")
	}
	return s.read.ListTxns(ctx, accountNo, from, to)
}

func (s *TxnService) GetBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	if s.read == nil {
		return domain.Balance{}, fmt.Errorf("txn: 未注入只读 store")
	}
	return s.read.GetLatestBalance(ctx, accountNo)
}
```

> **summary 注入说明**：`Post` 内 `summarize` 不填 summary（领域分录不含 summary）。上面在 Post 后给返回的 `txns[i].Summary` 赋值——但这只改了内存对象，**DB 里的 summary 仍为空**。修正：把 summary 通过 `Entry` 或 `Post` 参数传入。最小改动——给 `domain.Entry` 不加字段，而是让 `Record` 在 Post 之后用一次 UPDATE 写 summary。更干净的做法见 Step 4。

- [ ] **Step 4: 修正 summary 落库（避免 Post 后内存赋值不落库）**

给 `LedgerStore` 加一个轻量方法（在 `ledger_service.go` 接口与 `ledger_repo.go` 实现各加）。接口加：

```go
SetTxnSummary(ctx context.Context, q pg.DBTX, voucherNo, summary string) error
```

`ledger_repo.go` 实现：

```go
func (r *LedgerRepo) SetTxnSummary(ctx context.Context, q pg.DBTX, voucherNo, summary string) error {
	_, err := q.ExecContext(ctx, `UPDATE acct_txn SET summary=$2 WHERE voucher_no=$1`, voucherNo, summary)
	return err
}
```

同步给 `ledger_service_test.go` 的 `recordingLedgerStore` 加：

```go
func (f *recordingLedgerStore) SetTxnSummary(context.Context, pg.DBTX, string, string) error { f.calls++; return nil }
```

`txn_service.go` 的 Record 里把 `for i := range txns { txns[i].Summary = in.Summary }` 那段替换为：

```go
		if in.Summary != "" {
			if err := s.store.SetTxnSummary(ctx, q, voucherNo, in.Summary); err != nil {
				return err
			}
		}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/`
Expected: PASS（含 Task 5 posting 测试）。

- [ ] **Step 6: Commit**

```bash
git add templates/bank/internal/corebanking/service/txn_service.go \
        templates/bank/internal/corebanking/service/txn_service_test.go \
        templates/bank/internal/corebanking/service/ledger_service.go \
        templates/bank/internal/corebanking/service/ledger_service_test.go \
        templates/bank/internal/corebanking/repo/ledger_repo.go
git commit -m "feat(bank): B-3 service — txn_service.Record 记账（事务原子+透支+lock ordering）" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 7: 冲正 txn_service.Reverse

**Files:**
- Modify: `templates/bank/internal/corebanking/service/txn_service.go`
- Modify: `templates/bank/internal/corebanking/service/txn_service_test.go`

**Interfaces:**
- Consumes: `LedgerStore.GetTxnsByVoucher/UpdateTxnStatus/ApplyBalanceDeltas/UpsertGL`（Task 3）、`LedgerService.Post`（Task 3）、`domain.ReverseMode`（Task 1）。
- Produces: `Reverse(ctx, voucherNo, mode) (ReverseResult, error)`。

- [ ] **Step 1: 写冲正测试（先失败）**

在 `txn_service_test.go` 追加。需要一个能返回凭证流水的 fake store——扩展 `recordingLedgerStore` 让 `GetTxnsByVoucher` 可配置返回值。在 `recordingLedgerStore` 加字段：

```go
type recordingLedgerStore struct {
	calls       int
	txns        []domain.Txn
	deltas      []domain.BalanceDelta
	gl          *domain.GLBalance
	voucherTxns []domain.Txn // GetTxnsByVoucher 返回
	statusLog   []string     // 记录 UpdateTxnStatus 调用
}

func (f *recordingLedgerStore) GetTxnsByVoucher(context.Context, pg.DBTX, string) ([]domain.Txn, error) {
	f.calls++
	return f.voucherTxns, nil
}
func (f *recordingLedgerStore) UpdateTxnStatus(_ context.Context, _ pg.DBTX, _ string, st domain.TxnStatus) error {
	f.calls++
	f.statusLog = append(f.statusLog, string(st))
	return nil
}
func (f *recordingLedgerStore) SetTxnSummary(context.Context, pg.DBTX, string, string) error { f.calls++; return nil }
```

（替换 Task 3/6 里同名 fake 方法的旧版本——以本版为准，三个新方法签名齐全。）

测试：

```go
func TestReverse_Blue_ReversesStatusAndRollbackDeltas(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "1001", VoucherNo: "V1"},
		{TxnID: "T2", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "2011", VoucherNo: "V1"},
	}}
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)

	res, err := svc.Reverse(context.Background(), "V1", domain.ReverseBlue)
	if err != nil {
		t.Fatalf("蓝冲应成功: %v", err)
	}
	if res.Mode != "blue" || res.Status != string(domain.TxnStatusReversed) {
		t.Errorf("蓝冲结果不对: %+v", res)
	}
	if len(res.Txns) != 0 {
		t.Errorf("蓝冲不应产生新流水, got %d", len(res.Txns))
	}
	if len(store.statusLog) == 0 || store.statusLog[0] != "reversed" {
		t.Errorf("应 UpdateTxnStatus=reversed, got %v", store.statusLog)
	}
	if len(store.deltas) == 0 {
		t.Error("蓝冲应回滚 delta")
	}
}

func TestReverse_Red_PostsReverseEntries(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "1001", VoucherNo: "V1"},
		{TxnID: "T2", AccountNo: "D2", DCFlag: domain.DCCredit, Amount: domain.NewMoneyFromCents(10000), SubjectCode: "2011", VoucherNo: "V1"},
	}}
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)

	res, err := svc.Reverse(context.Background(), "V1", domain.ReverseRed)
	if err != nil {
		t.Fatalf("红冲应成功: %v", err)
	}
	if res.Mode != "red" || res.ReversedVoucherNo == "" {
		t.Errorf("红冲应有新 voucher: %+v", res)
	}
	// store.txns 累计含原 voucherTxns（fake 不真写）+ Post 产生的反向 2 条
	if len(store.txns) < 2 {
		t.Errorf("红冲应经 Post 产生反向分录, store.txns=%d", len(store.txns))
	}
}

func TestReverse_AlreadyReversed(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: []domain.Txn{
		{TxnID: "T1", AccountNo: "D1", DCFlag: domain.DCDebit, Amount: 10000, SubjectCode: "1001", VoucherNo: "V1", TxnStatus: domain.TxnStatusReversed},
	}}
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)
	_, err := svc.Reverse(context.Background(), "V1", domain.ReverseBlue)
	if !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("已冲正应 ErrAlreadyReversed, got %v", err)
	}
}

func TestReverse_NotFound(t *testing.T) {
	store := &recordingLedgerStore{voucherTxns: nil} // 空凭证
	svc := NewTxnService(nil, fakeAccountsRdr{}, NewLedgerService(store), store)
	_, err := svc.Reverse(context.Background(), "NOPE", domain.ReverseBlue)
	if !errors.Is(err, ErrVoucherNotFound) {
		t.Fatalf("凭证不存在应 ErrVoucherNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/ -run TestReverse`
Expected: FAIL（`Reverse` 未定义）。

- [ ] **Step 3: 实现 Reverse（追加到 txn_service.go）**

```go
// ReverseResult 冲正结果。
type ReverseResult struct {
	VoucherNo         string
	Mode              string
	Status            string // 蓝冲: reversed；红冲: 原 normal（不变）
	ReversedVoucherNo string // 红冲产生的反向凭证号；蓝冲为空
	Txns              []domain.Txn
}

// Reverse 冲正：blue=改状态+逆向delta回滚(不新增流水)；red=反向分录走Post(新增反向流水)。
// 一凭证只能冲一次。冲正本身不可再冲正。
func (s *TxnService) Reverse(ctx context.Context, voucherNo string, mode domain.ReverseMode) (ReverseResult, error) {
	bizDate := today()
	var res ReverseResult

	err := pg.RunInTx(ctx, s.db, func(q pg.DBTX) error {
		origs, err := s.store.GetTxnsByVoucher(ctx, q, voucherNo)
		if err != nil {
			return err
		}
		if len(origs) == 0 {
			return ErrVoucherNotFound
		}
		// 防重复：原凭证任一流水已 reversed → 拒绝
		for _, t := range origs {
			if t.TxnStatus == domain.TxnStatusReversed {
				return ErrAlreadyReversed
			}
		}
		ccy := origs[0].Ccy

		switch mode {
		case domain.ReverseBlue:
			if err := s.store.UpdateTxnStatus(ctx, q, voucherNo, domain.TxnStatusReversed); err != nil {
				return err
			}
			// 逆向 delta 回滚余额/总账（原 delta 的镜像：贷→借翻转符号）
			deltas, gl := reverseRollback(origs, bizDate)
			if err := s.store.ApplyBalanceDeltas(ctx, q, bizDate, deltas); err != nil {
				return err
			}
			if err := s.store.UpsertGL(ctx, q, gl); err != nil {
				return err
			}
			res = ReverseResult{VoucherNo: voucherNo, Mode: string(mode), Status: string(domain.TxnStatusReversed)}

		case domain.ReverseRed:
			newVoucher := domain.NewVoucherNo(bizDate)
			entries := reverseEntries(origs)
			// ref 关联原凭证代表流水（第一条）
			ref := origs[0].TxnID
			txns, err := s.ledger.Post(ctx, q, entries, bizDate, ccy, newVoucher, ref)
			if err != nil {
				return err
			}
			res = ReverseResult{VoucherNo: voucherNo, Mode: string(mode), Status: string(domain.TxnStatusNormal),
				ReversedVoucherNo: newVoucher, Txns: txns}

		default:
			return fmt.Errorf("txn: 未知冲正模式 %q", mode)
		}
		return nil
	})
	if err != nil {
		return ReverseResult{}, err
	}
	return res, nil
}

// reverseRollback 由原流水算逆向 BalanceDelta（原贷+→逆向-，原借-→逆向+）与镜像总账。
func reverseRollback(txns []domain.Txn, bizDate string) ([]domain.BalanceDelta, domain.GLBalance) {
	byAcct := map[string]domain.Money{}
	subj := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, t := range txns {
		if t.DCFlag == domain.DCCredit {
			byAcct[t.AccountNo] = byAcct[t.AccountNo].Sub(t.Amount) // 原贷+ → 逆向-
			glCC = glCC.Sub(t.Amount)
		} else {
			byAcct[t.AccountNo] = byAcct[t.AccountNo].Add(t.Amount) // 原借- → 逆向+
			glDC = glDC.Sub(t.Amount)
		}
		subj[t.AccountNo] = t.SubjectCode
	}
	deltas := make([]domain.BalanceDelta, 0, len(byAcct))
	for acct, d := range byAcct {
		deltas = append(deltas, domain.BalanceDelta{AccountNo: acct, Delta: d, SubjectCode: subj[acct]})
	}
	gl := domain.GLBalance{BizDate: bizDate, DCBalance: glDC, CCBalance: glCC, Ccy: txns[0].Ccy}
	if len(deltas) > 0 {
		gl.SubjectCode = deltas[0].SubjectCode
	}
	return deltas, gl
}

// reverseEntries 由原流水构造反向分录（dc_flag 翻转，金额不变）——红冲用，走 Post。
func reverseEntries(txns []domain.Txn) []domain.Entry {
	es := make([]domain.Entry, 0, len(txns))
	for _, t := range txns {
		flag := domain.DCCredit
		if t.DCFlag == domain.DCCredit {
			flag = domain.DCDebit
		}
		es = append(es, domain.Entry{AccountNo: t.AccountNo, DCFlag: flag, Amount: t.Amount, SubjectCode: t.SubjectCode})
	}
	return es
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/service/`
Expected: PASS（记账 + 冲正单测全过）。

- [ ] **Step 5: Commit**

```bash
git add templates/bank/internal/corebanking/service/txn_service.go templates/bank/internal/corebanking/service/txn_service_test.go
git commit -m "feat(bank): B-3 service — txn_service.Reverse 冲正（蓝冲改状态+逆向delta / 红冲反向分录）" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 8: HTTP handlers + router + DTO

**Files:**
- Modify: `templates/bank/internal/corebanking/api/handlers.go`
- Modify: `templates/bank/internal/corebanking/api/router.go`
- Modify: `templates/bank/internal/corebanking/api/handlers_test.go`

**Interfaces:**
- Consumes: `service.TxnService.Record/Reverse`（Task 6/7）、`domain.Action/ReverseMode`（Task 1）。
- Produces: `POST /api/v1/txns`、`POST /api/v1/vouchers/{voucher_no}/reverse`。

- [ ] **Step 1: 加 postBody helper 与 handler 测试（先失败）**

在 `handlers_test.go` 末尾追加（import 若缺补 `bytes`/`net/http`）：

```go
func postJSON(t *testing.T, h http.Handler, path, body string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// svcStub 最小 TxnService 替身：直接构造一个用 fake store 的真 TxnService。
func newRecordSvc() *service.TxnService {
	store := &recordingAPIStore{}
	return service.NewTxnService(nil, apiAccountsRdr{map[string]domain.DemandAccount{
		"D1": {AccountNo: "D1", SubjectCode: "2011", Ccy: "CNY", Status: domain.AccountStatusActive},
	}}, service.NewLedgerService(store), store)
}

type recordingAPIStore struct{}
// 实现 service.LedgerStore 全部方法（InsertTxns 回填假 ID，其余 no-op）... 见下

func TestPostTxn_Deposit_201(t *testing.T) {
	h := &Handlers{TxnSvc: newRecordSvc()}
	code, body := postJSON(t, NewRouter(h), "/api/v1/txns",
		`{"action":"deposit","account_no":"D1","amount":"100.00","ccy":"CNY"}`)
	if code != 201 || !strings.Contains(body, `"voucher_no"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestPostTxn_BadRequest_MissingAction(t *testing.T) {
	h := &Handlers{TxnSvc: newRecordSvc()}
	code, _ := postJSON(t, NewRouter(h), "/api/v1/txns", `{"account_no":"D1","amount":"1.00"}`)
	if code != 400 {
		t.Errorf("缺 action 应 400, got %d", code)
	}
}

func TestPostTxn_AccountNotFound_404(t *testing.T) {
	h := &Handlers{TxnSvc: newRecordSvc()}
	code, _ := postJSON(t, NewRouter(h), "/api/v1/txns",
		`{"action":"deposit","account_no":"NOPE","amount":"1.00","ccy":"CNY"}`)
	if code != 404 {
		t.Errorf("账户不存在应 404, got %d", code)
	}
}
```

> `recordingAPIStore` 与 `apiAccountsRdr` 须实现 `service.LedgerStore` 与 `service.AccountReader`。完整实现见 Step 3（与 service 包的 fake 同构）。为避免 api 测试 import service 内部 fake，本 task 在 api 包内定义最小 fake。实现：

```go
type apiAccountsRdr struct{ m map[string]domain.DemandAccount }
func (a apiAccountsRdr) GetDemand(_ context.Context, no string) (domain.DemandAccount, error) {
	if v, ok := a.m[no]; ok {
		return v, nil
	}
	return domain.DemandAccount{}, sql.ErrNoRows
}

type recordingAPIStore struct{}
func (recordingAPIStore) InsertTxns(_ context.Context, _ pg.DBTX, txns []domain.Txn) error {
	for i := range txns { txns[i].TxnID = "T-api" }
	return nil
}
func (recordingAPIStore) ApplyBalanceDeltas(context.Context, pg.DBTX, string, []domain.BalanceDelta) error { return nil }
func (recordingAPIStore) UpsertGL(context.Context, pg.DBTX, domain.GLBalance) error                       { return nil }
func (recordingAPIStore) EnsureBalanceRow(_ context.Context, _ pg.DBTX, _ string, _ string, _ string) (domain.Balance, error) {
	return domain.Balance{AvailableBalance: domain.NewMoneyFromCents(999999)}, nil
}
func (recordingAPIStore) GetTxnsByVoucher(context.Context, pg.DBTX, string) ([]domain.Txn, error) { return nil, nil }
func (recordingAPIStore) UpdateTxnStatus(context.Context, pg.DBTX, string, domain.TxnStatus) error { return nil }
func (recordingAPIStore) SetTxnSummary(context.Context, pg.DBTX, string, string) error            { return nil }
```

api 包测试 import 需补 `"bank/internal/platform/pg"`、`"database/sql"`、`"bank/internal/corebanking/service"`。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/api/`
Expected: FAIL（`PostTxn` 未定义 / 路由无 POST）。

- [ ] **Step 3: 加 handler + DTO（handlers.go 追加）**

在 `handlers.go` import 加 `"errors"`（已有则略）与 `"bank/internal/platform/pg"`（若 DTO 不需要可略）。在文件末尾追加：

```go
// PostTxn 记账：业务意图 → 复式过账。
func (h *Handlers) PostTxn(w http.ResponseWriter, r *http.Request) {
	var req postTxnReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("请求体非法 JSON"))); return
	}
	if req.Action == "" {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("缺少 action"))); return
	}
	if req.Amount == "" {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("缺少 amount"))); return
	}
	amt, err := domain.ParseCents(req.Amount)
	if err != nil || amt <= 0 {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("amount 非法（须元.分且>0）"))); return
	}
	in := service.RecordInput{
		Action: domain.Action(req.Action), Amount: amt, Ccy: req.Ccy, Summary: req.Summary,
		AccountNo: req.AccountNo, FromAccount: req.FromAccount, ToAccount: req.ToAccount,
	}
	booking, err := h.TxnSvc.Record(r.Context(), in)
	if err != nil {
		writeJSON(w, statusFor(err), errMap(err)); return
	}
	writeJSON(w, http.StatusCreated, bookingToResp(booking))
}

// ReverseVoucher 冲正：?mode=blue|red（默认 blue）。
func (h *Handlers) ReverseVoucher(w http.ResponseWriter, r *http.Request) {
	voucherNo := chiURLParam(r, "voucher_no")
	mode := domain.ReverseMode(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = domain.ReverseBlue
	}
	if mode != domain.ReverseBlue && mode != domain.ReverseRed {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("mode 须 blue 或 red"))); return
	}
	res, err := h.TxnSvc.Reverse(r.Context(), voucherNo, mode)
	if err != nil {
		writeJSON(w, statusFor(err), errMap(err)); return
	}
	writeJSON(w, http.StatusOK, reverseToResp(res))
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, service.ErrAccountNotFound), errors.Is(err, service.ErrVoucherNotFound):
		return http.StatusNotFound
	case errors.Is(err, service.ErrAccountNotActive), errors.Is(err, service.ErrAlreadyReversed):
		return http.StatusConflict
	case errors.Is(err, service.ErrInsufficientBalance):
		return http.StatusUnprocessableEntity
	case errors.Is(err, service.ErrCcyMismatch):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// --- DTO ---

type postTxnReq struct {
	Action      string `json:"action"`
	AccountNo   string `json:"account_no"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	Amount      string `json:"amount"`
	Ccy         string `json:"ccy"`
	Summary     string `json:"summary"`
}

type txnLineResp struct {
	TxnID       string `json:"txn_id"`
	AccountNo   string `json:"account_no"`
	DCFlag      string `json:"dc_flag"`
	Amount      string `json:"amount"`
	SubjectCode string `json:"subject_code"`
	VoucherNo   string `json:"voucher_no,omitempty"`
	RefTxnID    string `json:"ref_txn_id,omitempty"`
}

type recordResp struct {
	VoucherNo string        `json:"voucher_no"`
	BizDate   string        `json:"biz_date"`
	Txns      []txnLineResp `json:"txns"`
}

type reverseResp struct {
	VoucherNo         string        `json:"voucher_no"`
	Mode              string        `json:"mode"`
	Status            string        `json:"status,omitempty"`
	ReversedVoucherNo string        `json:"reversed_voucher_no,omitempty"`
	Txns              []txnLineResp `json:"txns,omitempty"`
}

func bookingToResp(b domain.Booking) recordResp {
	out := recordResp{VoucherNo: b.VoucherNo, BizDate: b.BizDate, Txns: make([]txnLineResp, 0, len(b.Txns))}
	for _, t := range b.Txns {
		out.Txns = append(out.Txns, txnLineResp{
			TxnID: t.TxnID, AccountNo: t.AccountNo, DCFlag: string(t.DCFlag),
			Amount: t.Amount.String(), SubjectCode: t.SubjectCode, VoucherNo: t.VoucherNo,
		})
	}
	return out
}

func reverseToResp(r service.ReverseResult) reverseResp {
	out := reverseResp{VoucherNo: r.VoucherNo, Mode: r.Mode, Status: r.Status, ReversedVoucherNo: r.ReversedVoucherNo}
	for _, t := range r.Txns {
		out.Txns = append(out.Txns, txnLineResp{
			TxnID: t.TxnID, AccountNo: t.AccountNo, DCFlag: string(t.DCFlag),
			Amount: t.Amount.String(), SubjectCode: t.SubjectCode, VoucherNo: t.VoucherNo, RefTxnID: t.RefTxnID,
		})
	}
	return out
}
```

`handlers.go` 顶部 import 确保含 `"bank/internal/corebanking/service"`（已有）。

- [ ] **Step 4: 加路由（router.go）**

在 `router.go` 的 `r.Route("/api/v1", ...)` 块内追加两行（与现有 GET 并列）：

```go
		r.Route("/api/v1", func(r chi.Router) {
			r.Get("/accounts/{account_no}", h.GetAccount)
			r.Get("/accounts/{account_no}/balance", h.GetBalance)
			r.Get("/txns", h.ListTxns)
			r.Get("/ledger", h.GetLedger)
			r.Post("/txns", h.PostTxn)                                  // B-3 记账
			r.Post("/vouchers/{voucher_no}/reverse", h.ReverseVoucher)  // B-3 冲正
		})
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test ./internal/corebanking/api/`
Expected: PASS（含原有只读测试 + 新写测试）。

- [ ] **Step 6: Commit**

```bash
git add templates/bank/internal/corebanking/api/handlers.go \
        templates/bank/internal/corebanking/api/router.go \
        templates/bank/internal/corebanking/api/handlers_test.go
git commit -m "feat(bank): B-3 api — PostTxn/ReverseVoucher handler + 路由 + 错误码映射" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 9: main.go 依赖装配 + 编译

**Files:**
- Modify: `templates/bank/cmd/core-banking/main.go`

**Interfaces:**
- Consumes: `NewTxnService`（Task 6）、`LedgerService`（Task 3）。
- Produces: 可运行的 core-banking，写端点生效。

- [ ] **Step 1: 改 main.go 装配**

把 `cmd/core-banking/main.go` 的 `handlers := &api.Handlers{...}` 块替换为：

```go
	ledgerRepo := repo.NewLedgerRepo(db)
	ledgerSvc := service.NewLedgerService(ledgerRepo)
	txnRepo := repo.NewTxnRepo(db)
	txnSvc := service.NewTxnService(db, repo.NewAccountRepo(db), ledgerSvc, ledgerRepo).WithReader(txnRepo)

	handlers := &api.Handlers{
		Accounts: repo.NewAccountRepo(db),
		TxnSvc:   txnSvc,
		Ledger:   ledgerRepo,
	}
```

- [ ] **Step 2: 全量编译 + 单测**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && go test ./...`
Expected: BUILD + TEST 全绿（不含 integration tag）。

- [ ] **Step 3: Commit**

```bash
git add templates/bank/cmd/core-banking/main.go
git commit -m "feat(bank): B-3 main — core-banking 装配写依赖（TxnService 注入 db/ledger/store）" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

### Task 10: 集成测试（并发死锁 + 事务原子）+ e2e + templates.tar 重打包

**Files:**
- Modify: `templates/bank/internal/corebanking/repo/integration_test.go`
- Run: e2e 冒烟（手动/curl）、`go generate`（jiade 仓根）

**Interfaces:**
- 验收：并发 A→B / B→A 不死锁；记账→冲正 e2e 链路；templates.tar 重打包。

- [ ] **Step 1: 加并发死锁集成测试**

在 `integration_test.go` 追加：

```go
func TestRecord_Concurrent_NoDeadlock(t *testing.T) {
	db := setupDB(t)
	defer db.Close()
	ctx := context.Background()
	ar := repo.NewAccountRepo(db)
	lr := repo.NewLedgerRepo(db)
	svc := service.NewTxnService(db, ar, service.NewLedgerService(lr), lr)

	for _, no := range []string{"CD-A", "CD-B"} {
		db.ExecContext(ctx, "DELETE FROM acct_txn WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM account_balance WHERE account_no=$1", no)
		db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no=$1", no)
		ar.InsertDemand(ctx, domain.DemandAccount{
			AccountNo: no, CustID: "C", Ccy: "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: "2026-07-15", SubjectCode: "2011",
		})
		db.ExecContext(ctx, `INSERT INTO account_balance (account_no,biz_date,balance,available_balance,subject_code)
			VALUES ($1,'2026-07-15',1000000,1000000,'2011')`, no) // 各 10000.00
	}

	errs := make(chan error, 2)
	// T1: A→B；T2: B→A —— 若无 lock ordering 会 AB-BA 死锁
	go func() { _, e := svc.Record(ctx, service.RecordInput{Action: domain.ActionTransfer, FromAccount: "CD-A", ToAccount: "CD-B", Amount: domain.NewMoneyFromCents(10000), Ccy: "CNY"}); errs <- e }()
	go func() { _, e := svc.Record(ctx, service.RecordInput{Action: domain.ActionTransfer, FromAccount: "CD-B", ToAccount: "CD-A", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY"}); errs <- e }()

	for i := 0; i < 2; i++ {
		if e := <-errs; e != nil {
			t.Fatalf("并发转账失败: %v", e)
		}
	}
	// 两笔都成功：A 余额 10000-100+50=9950.00；B 余额 10000+100-50=10050.00
	tr := repo.NewTxnRepo(db)
	ba, _ := tr.GetLatestBalance(ctx, "CD-A")
	bb, _ := tr.GetLatestBalance(ctx, "CD-B")
	if ba.Balance != domain.NewMoneyFromCents(995000) {
		t.Errorf("A 余额=%s, want 9950.00", ba.Balance)
	}
	if bb.Balance != domain.NewMoneyFromCents(1005000) {
		t.Errorf("B 余额=%s, want 10050.00", bb.Balance)
	}
}
```

import 补 `"bank/internal/corebanking/service"`。

- [ ] **Step 2: 跑集成测试**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go test -tags integration ./...`
Expected: PASS（pg 未就绪则 skip；就绪时并发不死锁、余额正确）。若偶发死锁（40P01），检查 `lockedAccountList` 是否升序。

- [ ] **Step 3: e2e 冒烟（需 pg + 服务起）**

```bash
cd templates/bank
make up && go run ./cmd/seed --scale=dev --reset
# 起服务（后台）
go run ./cmd/core-banking &
sleep 2
# 取一个 seed 账户（如 D0000000001）存款
curl -s -X POST localhost:8080/api/v1/txns -d '{"action":"deposit","account_no":"D0000000001","amount":"100.00","ccy":"CNY"}'
# 查余额（应增加 100）
curl -s localhost:8080/api/v1/accounts/D0000000001/balance
# 取 voucher_no 后冲正（蓝冲）
VOUCHER=$(curl -s -X POST localhost:8080/api/v1/txns -d '{"action":"withdraw","account_no":"D0000000001","amount":"50.00","ccy":"CNY"}' | grep -o '"voucher_no":"[^"]*"' | cut -d'"' -f4)
curl -s -X POST "localhost:8080/api/v1/vouchers/$VOUCHER/reverse?mode=blue"
# 查余额（withdraw 50 被蓝冲回滚）
curl -s localhost:8080/api/v1/accounts/D0000000001/balance
kill %1
```
Expected: 记账 201、余额随 deposit/withdraw 变化、蓝冲后余额复原。

- [ ] **Step 4: 全量验证（无 integration tag）**

Run: `cd templates/bank && CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0 go build ./... && go test ./...`
Expected: BUILD + TEST 全绿。

- [ ] **Step 5: 重打包 templates.tar（jiade 仓根）**

Run: `go generate ./internal/template`
Expected: `internal/template/templates.tar` 更新（Makefile test/e2e 依赖它）。

Run（jiade 自身）: `go build ./... && go test ./...`
Expected: 全绿。

- [ ] **Step 6: Commit**

```bash
git add templates/bank/internal/corebanking/repo/integration_test.go internal/template/templates.tar
git commit -m "test(bank): B-3 集成 — 并发 A→B/B→A 不死锁 + 重打包 templates.tar" \
  -m "Co-Authored-By: Claude <noreply@anthropic.com>"
```

---

## Self-Review 记录（plan 作者已执行）

**1. Spec 覆盖**：
- §2 目标 1 记账 → Task 5+6+8。✓
- §2 目标 2 冲正 → Task 7+8。✓
- §2 目标 3 数据模型 → Task 1。✓
- §2 目标 4 并发原子 → Task 2+3（EnsureBalanceRow/事务）+10（并发测试）。✓
- §2 目标 5 Post 改造 → Task 3。✓
- §2 目标 6 测试 → Task 3/5/6/7/8/10。✓
- §7.1 科目规则 → Task 5。✓ §7.2 透支 → Task 6。✓ §7.3 蓝/红冲 → Task 7。✓
- §8 并发/事务 → Task 2/3/6/10。✓ §9 错误码 → Task 8 `statusFor`。✓
- §11 验收 1-10 → 各 task + Task 10。✓

**2. 占位符/笔误**：已修正——Task 3 Step 4 `TxnStatus` 字段名笔误；Task 7 `reverseRollback` 的 `glDC` 逆向方向 bug（原误写 `Add`，借方发生额回滚应为 `Sub`，否则总账不回滚反加倍）。其余无 TBD/TODO。

**额外提示**：Task 6 `Record` 的主账户解析当前为「先取 `AccountNo`，transfer 且其为空时取 `FromAccount`」——依赖「transfer 请求不填 `account_no`」的隐含约定；实现时可简化为 `primaryNo := in.FromAccount(if transfer) else in.AccountNo` 单次取，逻辑等价更清晰。

**3. 类型一致性**：
- `LedgerStore` 接口跨 Task 3（定义）/6（用）/7（用）/8（api fake）一致：6 方法 + SetTxnSummary（Task 6 Step 4 加）。**注意**：Task 6 Step 4 加了 `SetTxnSummary`，故 Task 8 的 `recordingAPIStore` 与 Task 7 的 `recordingLedgerStore` 都含它（已写）。
- `Post` 签名 `(ctx, q, entries, bizDate, ccy, voucherNo, refTxnID) ([]Txn, error)` 全链路一致。✓
- `Record`/`Reverse`/`ReverseResult`/`RecordInput` 一致。✓
- `BuildEntries(action, acct, counterparty, amount)` 一致。✓

**已知简化（与 spec §12 一致）**：无幂等、biz_date 取当天、冲正不可再冲正、EnsureBalanceRow 为运行时即时滚存（非 B-2 逐日引擎）。
