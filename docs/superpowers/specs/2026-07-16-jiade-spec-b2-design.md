# Jiade — Spec B-2 设计文档

- **日期**：2026-07-16
- **状态**：设计待审（待用户复审）
- **范围**：Spec B-2 —— core_db 多日切日引擎（`bizdate` + `distribution` 两模块移植，替换 Spec A 一次性快照 fixture）
- **作者**：yuhaochen × Claude（brainstorming）

---

## 1. 背景与定位

### 1.1 Spec A 的"一次性快照"与 B-2 的替代

Spec A 的 core fixture 是**快照式**：`GenBalanceRows` 只给每个活期账户写**一条 EndBizDate 余额快照**；`GenTxnRows` 只生成**最近 5 天、每日 5 笔**流水。biz_date 散布在范围内，但**非逐日滚存**——无趋势、无季节性、无余额跨日继承，同环比无真实波动。

B-2 移植 `bizdate.py`（逐日推进引擎）+ `distribution.py`（trend/seasonal/cyclical 三因子），把这块换成**逐日滚存引擎**：`[StartBizDate, EndBizDate]` 全量日历日逐日生成 `acct_txn`、写当日全账户 `account_balance` 快照、内存滚余额，末尾切 `sys_param.biz_date`。

### 1.2 Spec B 的分解（B-1/B-3 已合并 main）

| 子 spec | 范围 | 依赖 | 状态 |
|---------|------|------|------|
| B-1 | customer + payment + 多库 + FDW | Spec A | ✅ 已合并 main |
| B-3 | core-banking 写 HTTP 接口（记账/冲正） | Spec A service 层 | ✅ 已合并 main |
| **B-2** | **多日切日引擎（bizdate+distribution 移植）** | **Spec A core_db** | **本 spec** |
| B-4 | 剩余 4 服务（reward/risk/loan/wealth） | B-1 | ⬜ |

B-2 只依赖 Spec A，与 B-1/B-4 互相独立；含**一处对已合并 B-3 的衔接改动**（B-3 spec §14 预告：记账 biz_date 改读 `sys_param.biz_date`）。

### 1.3 范围限定 core_db

loan/wealth 的多日数据（`loan_balance`/`wealth_nav` 等 biz_date 维度）随 B-4 各域。本 spec 只做 core_db 的 `acct_txn` / `account_balance` / `sys_param`。

---

## 2. 目标与非目标

### 2.1 B-2 目标

1. **distribution 三因子**：trend（每月 +2%）× seasonal（季末/年末/发薪日/节假日 spike）× cyclical（周末 ×0.60），乘性叠加决定每日交易量。
2. **逐日滚存引擎**：`[2025-06-01, 2026-07-13]` 全 ~408 个日历日（含周末），每日生成单腿流水（贷多借少→余额温和增长）、内存滚余额、写当日**全账户**余额快照。
3. **切日**：seed 末 `sys_param.biz_date = EndBizDate`（2026-07-13）。
4. **B-3 衔接**：`txn_service.Record/Reverse` 的 biz_date 从 `time.Now()` 改读 `sys_param.biz_date`。
5. **确定性**：同 Seed+Scale+日期范围 → 同样的行；每日独立 rng，任意子范围可复现。

### 2.2 B-2 非目标

- **gl_balance 历史日**：不补。切日引擎不写 gl_balance；且历史流水是**单腿**（transaction-log 式，非复式凭证），无法有意义地汇总成平衡总账。`gl_balance` 仍只由 B-3 实时复式记账产生（`/ledger` 历史日返回空，与现状一致）。
- **跳过周末/节假日**：不跳，靠 cyclical/seasonal 因子压量（保留同环比连续性）。
- **loan/wealth 多日数据**：B-4。
- **复式凭证历史流水**：种子历史是单腿 transaction-log（空 `voucher_no`），与 B-3 实时复式凭证共存（B-3 §5 已预留读接口容错）。
- **账户开闭户动态**：全量账户从 StartBizDate 即活跃；不做"开户晚于 d 则不交易"的精细化。
- **真实日终切日运维命令**：B-2 是 seed 批量生成历史日，非运行时切日；未来若需可另包 CLI。

---

## 3. 核心原则（延续 Spec A/B-1/B-3）

| 原则 | 在 B-2 的体现 |
|------|---------------|
| **金额 int64 分，禁 float** | 流水金额/余额全程 `domain.Money`（分）。三因子的 factor 是 float（仅缩放 int 交易量级，非金额，无精度问题）。 |
| **确定性** | 每日独立 rng（`seed+偏移+ordinal`）+ 确定 txn_id；DeepEqual 两次跑比对（对齐现有 `core_test.go` 范式）。 |
| **生成物自包含 / 自闭** | 引擎在 bank module 内，零外部依赖；jiade 不联动 SCV/Porto。 |
| **依赖方向向内** | 引擎属 `fixtures`（最外生成层），可 import `domain`；不改 `domain`。 |
| **复式记账只在 core** | B-2 只生成 core 单腿历史流水；写接口（B-3）经 `LedgerService.Post`。 |

---

## 4. 架构总览（方案 3：纯内核 + 薄写入循环）

引擎拆成**纯生成内核**（无 DB，可确定性单测）+ **薄写入循环**（逐日批量落库 + 切日）。

### 4.1 文件落点

```
templates/bank/internal/fixtures/domains/
├── core.go          # GenStaticData / GenAccountRows 保留
│                    # GenBalanceRows / GenTxnRows / WriteBalances / WriteTxns 删除（被引擎取代）
├── bizdate.go       # 新：distribution 三因子 + DayState + GenDay(纯) + RunBizDate(写入循环) + 批量 writer（均 package domains）
└── bizdate_test.go  # 新：因子单测 + GenDay 确定性 + DayState 滚存 + 全量稳定性

templates/bank/cmd/seed/main.go          # step3 core：static→accounts→ bizdate.Run(...)
templates/bank/internal/corebanking/
├── repo/ledger_repo.go                  # +GetBizDate(ctx)
└── service/txn_service.go               # today()→s.store.GetBizDate(ctx)
```

### 4.2 数据流

```
seed step3 core:
  GenStaticData → WriteStatic
  GenAccountRows → WriteAccounts
  domains.RunBizDate(ctx, coreDB, cfg, demandNos)   # bizdate.go，package domains
    ├─ 初始化 DayState（每账户确定初始余额，rng seed+2）
    ├─ for d in [Start..End]（日历日，含周末）:
    │    GenDay(cfg, d, &state) → (dayTxns, dayBalances)     # 纯函数
    │    tx (pg.RunInTx):
    │       DELETE FROM acct_txn       WHERE biz_date=d
    │       批量 INSERT dayTxns
    │       DELETE FROM account_balance WHERE biz_date=d
    │       批量 INSERT dayBalances
    └─ UPDATE sys_param.biz_date=EndBizDate（+prev_biz_date=次末日）
```

customer / payment / setup_fdw 步骤**不动**；seed 编排仍 6 步。

---

## 5. distribution 三因子（移植 `distribution.py`）

落 `bizdate.go`，纯函数（可单测，无 DB）：

| 因子 | 规则 |
|------|--------------------|
| `trendFactor(d)` | `1.0 + 0.02 × 月数`，月数 = 自 base `2025-06-01` 起的整月差 |
| `seasonalFactor(d)` | 季末（月∈{3,6,9,12} 且日≥28）×1.35；年末（12 月且日≥25）×1.50；发薪日（日∈{10,15}）×1.40；节假日×1.30（可叠加乘） |
| `cyclicalFactor(d)` | 周末（weekday≥5）×0.60，否则 1.0 |
| `holidays` | 国庆 2025-10-01/02/03、元旦 2026-01-01、春节 2026-02-16/17/18、双十一 2025-10-10 / 2025-11-11 |

`volumeForDay(cfg, d) int`：

- 每日独立 rng = `NewRNG(cfg.Seed + 100 + ordinal(d))`
- `factor = trendFactor(d) × seasonalFactor(d) × cyclicalFactor(d)`
- `lo = max(1, int(baseLo × factor))`；`hi = max(lo+1, int(baseHi × factor))`
- 返回 `rng.IntRange(lo, hi)`

→ 单日结果**只依赖日期本身**，子范围重跑/跨日推进时单日可复现。

> factor 是 float，仅用于缩放 int 交易量上下界——**不触碰金额**，无 float 精度问题。

---

## 6. 引擎内核 `GenDay`（纯函数）

```go
// DayState 账户余额的内存滚存态（避免逐笔查库）。
type DayState struct { Bal map[string]domain.Money } // account_no → 余额（分）

// GenDay 生成当日流水 + 当日全账户余额快照，并推进 state。纯函数（不碰 DB）。
func GenDay(cfg fixtures.Config, date string, st *DayState) (dayTxns []domain.Txn, dayBalances []domain.Balance)
```

每日流程：

1. `n = volumeForDay(cfg, date)`
2. 内容 rng = `NewRNG(cfg.Seed + 200 + ordinal(date))`（**每日独立**，比顺序 fk 更鲁棒——任意子范围可复现）
3. `for i in [0, n)`：
   - 随机账户 `demandNos[rng.IntRange(0, len-1)]`
   - 金额 `domain.NewMoneyFromCents(int64(rng.IntRange(1, 9999)) * 1000)` 分
   - dc 加权：贷 2/3、借 1/3（`IntRange(0,2)`：0/1=贷，2=借）
   - 余额推进：贷→`st.Bal[acct] = st.Bal[acct].Add(amt)`；借→`st.Bal[acct] = max0(st.Bal[acct].Sub(amt))`（clamp 0）
   - `opp_account` 随机 demandNo；`channel`/`summary` 取 Jiade 词库（`fixtures.Channels`/`fixtures.Summaries`）
   - `txn_id = "T" + dateCompact(date) + "-" + fmt.Sprintf("%05d", i)`（**确定性**，非 crypto-rand；日期+日内序保证全局唯一）
   - `subject_code = "2011"`，`ccy = "CNY"`，`voucher_no = ""`，`txn_status = "normal"`
4. `dayBalances`：遍历 `st.Bal` **全账户**，盖 `date` 戳生成快照（`available=balance`，`frozen=0`，`subject="2011"`）。

**金额常量**沿用原型量级（plan 阶段可微调）：

| 项 | 原型（元，float） | Jiade（分，int64） |
|----|--------------------|--------------------|
| 初始余额 | `round(random_number(5)×100, 2)` | `IntRange(1,99999) × 10000` 分 |
| 流水金额 | `round(random_number(4)×10, 2)` | `IntRange(1,9999) × 1000` 分 |

---

## 7. 写入循环 `Run`（批量 + 逐日幂等 + 切日）

```go
// RunBizDate 逐日滚存引擎入口（package domains，bizdate.go）。
func RunBizDate(ctx context.Context, db *sql.DB, cfg fixtures.Config, demandNos []string) error
```

1. 初始化 `DayState`：rng = `NewRNG(cfg.Seed + 2)`，每个 demandNo 一个确定初始余额（偏移 `+2` 由删除的 `GenBalanceRows` 回收，不与 `GenAccountRows` 的 `+1` 冲突）。
2. 逐日 `d ∈ [StartBizDate, EndBizDate]`（日历日，含周末）：
   - `dayTxns, dayBalances = GenDay(cfg, d, &state)`
   - 在一个 `pg.RunInTx` 内：
     - `DELETE FROM acct_txn WHERE biz_date=$1`
     - 批量 INSERT `dayTxns`（多行 `VALUES`，分块）
     - `DELETE FROM account_balance WHERE biz_date=$1`
     - 批量 INSERT `dayBalances`（多行 `VALUES`，分块）
3. 末尾 `UPDATE sys_param SET param_value=$1 WHERE param_key='biz_date'`（=EndBizDate）+ `prev_biz_date`（=次末日）。**只切一次**（逐日切冗余，最终结果相同）。

**批量 INSERT 分块**：构造多行 `VALUES ($1,$2,...),(...)`，规避 pg 65535 参数限——`acct_txn` 13 列 ≤5000 行/批；`account_balance` 6 列 ≤10000 行/批。

**逐日幂等**：当日 `DELETE` 后 `INSERT`，同日/子范围重跑安全。整体重跑由 `--reset` DROP 库保证。

---

## 8. seed 编排集成（替换快照）

`cmd/seed/main.go` step3（core）由

```go
domains.WriteBalances(ctx, coreDB, domains.GenBalanceRows(cfg, demandNos))
domains.WriteTxns(ctx, coreDB, domains.GenTxnRows(cfg, demandNos))
```

改为

```go
domains.RunBizDate(ctx, coreDB, cfg, demandNos) // bizdate.go，package domains
```

static / accounts 生成与 customer / payment / setup_fdw 步骤**不动**；编排仍 6 步。

`domains/core.go` 删除 `GenBalanceRows` / `GenTxnRows` / `WriteBalances` / `WriteTxns` 及其测试（`core_test.go` 中对应 3 个测试）。**删除将在实现阶段按"删除需确认"规则再次确认。**

---

## 9. 与 B-3 衔接（读 sys_param.biz_date）

- **repo**：`repo/ledger_repo.go` 增 `GetBizDate(ctx) (string, error)` —— `SELECT param_value FROM sys_param WHERE param_key='biz_date'`（用 `r.db`，无需事务）。
- **接口**：`service.LedgerStore` 增 `GetBizDate(ctx) (string, error)`。
- **service**：`txn_service.Record`（`:66`）与 `Reverse`（`:185`）把 `bizDate := today()` 改为 `bizDate, err := s.store.GetBizDate(ctx)`；删除/保留 `today()` 视实现（倾向删除，避免误用）。
- **缺失兜底**：`biz_date` 为空或查不到 → 返回明确错误「sys_param.biz_date 未设置，请先 seed」（**不**静默回退 `time.Now()`）。
- **合流语义**：seed 后 `sys_param.biz_date = 2026-07-13`；B-3 实时记账落该日，与 B-2 已写的 EndBizDate 余额快照经 `ApplyBalanceDeltas` 的 `ON CONFLICT 累加` 自然合流；`EnsureBalanceRow` 当天已有行直接 `FOR UPDATE`（无需跨日继承）。
- **测试影响**：B-3 单测的 fake store 增补 `GetBizDate` stub（返回固定日，如 `"2026-07-13"`）。

---

## 10. 确定性设计

| 来源 | rng | 说明 |
|------|-----|------|
| 初始余额 | `seed + 2` | 一次性；回收已删 `GenBalanceRows` 的偏移 |
| 当日交易量 | `seed + 100 + ordinal` | 与原型相同 |
| 当日流水内容 | `seed + 200 + ordinal` | **每日独立**（原型用顺序 fk；独立化使任意子范围可复现）|
| txn_id | `T<dateCompact>-<seq:05d>` | 确定、全局唯一（日期 + 日内序）|

三组偏移互不重叠，且避开 `GenAccountRows`（`+1`）、customer（`+10`）、payment（`+20`）。

---

## 11. 错误处理（对齐 Spec A §10）

- **引擎写失败**（pg 未起 / 批量 INSERT 错）：透出 pg 原始错误 + 非零退出；当日 `pg.RunInTx` 回滚，`--reset` 重跑安全。
- **`GetBizDate` 缺失**：记账返回明确错误（§9），不静默回退。
- **`--reset`**：仍是所有破坏性操作的显式确认（DROP 3 库重建）。

---

## 12. 测试策略（对齐 Spec A/B-1/B-3 §9）

1. **因子单测**（纯，无 DB）：`trendFactor` / `seasonalFactor` / `cyclicalFactor` / `volumeForDay` 在已知日期断言（季末日、周末、发薪日、节假日、普通日各一例）。
2. **GenDay 确定性**（纯）：固定 date 两次 `GenDay` → `reflect.DeepEqual`；txn_id 非空且日内唯一；dc 加权近似 2:1。
3. **DayState 滚存**（纯）：脚本化若干笔（含贷/借/clamp 0）→ 断言推进后余额。
4. **全量稳定性**（纯）：全 ~408 日 gen 两次 → 比对**聚合摘要**（总笔数、`Σ|amount|`、终态 `DayState` map）。比对全量行 DeepEqual 过重，用聚合摘要代替。
5. **集成**（`//go:build integration`，真 pg）：`Run` 后断言：
   - 逐日行数在 `[lo,hi]` 区间；
   - `sys_param.biz_date = 2026-07-13`；
   - 周末日均交易量 < 工作日（cyclical 生效）；
   - 季末日量显著高于普通日（seasonal 生效）；
   - 子范围重跑幂等（同日行不变）。
6. **衔接**（集成）：seed 后 `GetBizDate` 可读且为 EndBizDate；一次 `Record` 落在 2026-07-13、余额经 `ApplyBalanceDeltas` 累加正确。
7. **e2e**（Makefile/CI）：`seed → 校验多日数据存在 → 记账 → 查余额`。

---

## 13. 验收标准

1. jiade 仓自身：`go build ./...` / `go test ./...` 全绿（不含 `templates/`）。
2. `templates/bank` 独立 module：拷到临时目录后 `go build ./... && go test ./...` 全绿（CI 单独步骤）。
3. seed 后 core_db：`acct_txn` 跨 `[2025-06-01, 2026-07-13]` 全 ~408 日有数据；`account_balance` 每日每活期账户一条；`sys_param.biz_date = 2026-07-13`。
4. 周末日均交易量 < 工作日（cyclical 生效）；季末日量显著高于普通日（seasonal 生效）。
5. 同 Seed+Scale 两次 seed 产出一致（确定性单测通过）。
6. 子范围重跑（同日 `DELETE`+`INSERT`）幂等、行不变。
7. B-3 记账 biz_date 取自 `sys_param.biz_date`（非当天）；seed 后记账落在 2026-07-13。
8. 生成物自包含：未装 jiade（仅 docker+go）可 `docker compose up` + `go run ./cmd/seed`。

---

## 14. 已知简化（明确记录）

- **历史流水单腿**：非复式凭证，空 `voucher_no`；与 B-3 实时复式凭证共存。
- **gl_balance 历史日不补**：单腿流水无法汇总成平衡总账。
- **周末/节假日不跳过**：靠 cyclical/seasonal 因子压量。
- **sys_param 只切一次**到 EndBizDate（非逐日切；最终结果相同）。
- **无账户开闭户动态**：全量账户从 StartBizDate 即活跃。
- **daily_txn 量级沿用原型**：dev (500,1250) / full (2000,5000)；dev 总量约 35.7 万 txn + 81.6 万余额行（忠实原型，未裁剪）。

---

## 15. 必须延续的 Spec A/B-1/B-3 约束

- **自闭**：jiade 不联动 SCV/Porto，无 doctor。
- **生成物自包含**：bank 工程离开 jiade 可 `docker compose up` + `go run`。
- **金额 int64 分，禁 float**（`domain.Money` 全链路；三因子 factor 例外，仅缩放 int 量级）。
- **复式记账只在 core**：历史流水单腿；写接口（B-3）经 `LedgerService.Post`。
- **依赖方向向内** `api → service → repo → domain`；repo 不 import service；domain 零依赖。
- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`），不参与 jiade build；改后须 `go generate ./internal/template` 重新打包 `templates.tar`（Makefile test/e2e 已依赖 generate）。
- **go 1.22**：bank module `go.mod` pin 1.22；本地验证 macOS 15 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`。
- **删除需确认**：`--reset`/`--force` + 删除旧 fixture 函数均为破坏性操作的显式确认。

---

## 16. 后续衔接

- **B-4**：loan/wealth 各域的 biz_date 维度多日数据（`loan_balance`/`wealth_nav`）可复用本引擎的 distribution 三因子 + 逐日滚存模式。
- **真实切日命令**（未来）：若需"日终切日"运维命令（非 seed 批量），可在引擎上包一层 `cutover` CLI，B-2 不做。

---

## 17. 开放问题

- 无待决问题。日历（408 全量日历日 / 周末 ×0.60 压量）、引擎形态（方案 3：纯内核 + 薄写入循环）、替换 Spec A 快照、B-3 衔接（甲：读 `sys_param.biz_date`）、gl_balance 跳过、确定性（每日独立 rng + 确定 txn_id）均已确认。
