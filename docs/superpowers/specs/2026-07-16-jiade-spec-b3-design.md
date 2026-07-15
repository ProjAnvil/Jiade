# Jiade — Spec B-3 设计文档

- **日期**：2026-07-16
- **状态**：设计待审
- **范围**：Spec B-3 —— core-banking 写 HTTP 接口（记账 + 冲正），复式记账引擎内部化
- **作者**：yuhaochen × Claude（brainstorming）

---

## 1. 背景与定位

### 1.1 Spec A §7.4 的缺口

Spec A §7.4 明确：「写操作（过账/记账）在 service 层实现并有单测，但**暂不暴露 HTTP 写接口**（避免与 seed 的数据权威性冲突）；写接口留 Spec B」。B-3 正是补这个缺口——把 core-banking 的写能力暴露为 HTTP。

### 1.2 Spec B 的分解（B-1 已确认）

| 子 spec | 范围 | 依赖 | 体量 | 状态 |
|---------|------|------|------|------|
| B-1 | customer + payment + 多库 + FDW | Spec A | 中 | ✅ 已合并 main |
| **B-3** | **core-banking 写 HTTP 接口（记账/冲正）** | **Spec A service 层** | **小** | **本 spec** |
| B-2 | 多日切日引擎 | Spec A core_db | 中 | ⬜ |
| B-4 | 剩余 4 服务 | B-1 | 大 | ⬜ |

B-3 只依赖 Spec A，与 B-1/B-2/B-4 互相独立。

### 1.3 关键架构决策：API 面向业务，复式引擎内部化

**取消「过账」公开端点**。API 只暴露业务级的 transaction（记账/冲正）；复式记账引擎（`LedgerService.Post`，借必等于贷）降级为**内部基础设施**，被记账/冲正复用，不再有公开 HTTP 端点。

复式不变量从 Spec A 预期的「HTTP 层展示」转为「**内部保证**」——客户/柜员从不关心借贷平衡，那是会计内部的事。这更贴近真实银行：柜员看到的是「存一笔/转一笔」，复式分录由系统在内部自动生成。

### 1.4 Spec A 留下的三个真实缺口（B-3 一并解决）

| # | 缺口 | 现状 | B-3 解决方式 |
|---|------|------|--------------|
| 1 | **Post 三步落库无事务** | `InsertTxns → ApplyBalanceDeltas → UpsertGL` 顺序执行，中途失败留半写 | 引入事务封装，记账/冲正的「读-校验-写」整体在一个 DB 事务内 |
| 2 | **记账/冲正 service 不存在** | `txn_service.go` 只有只读 ListTxns/GetBalance | 新增 `Record`（记账）、`Reverse`（冲正） |
| 3 | **Post 不返回 TxnID** | repo 用 crypto/rand 生成随机 ID，service/handler 拿不到 | Post 改造为返回 txns；记账/冲正响应回传 txn_id / voucher_no |

---

## 2. 目标与非目标

### 2.1 B-3 目标

1. **记账端点** `POST /api/v1/txns`：业务意图驱动（deposit/withdraw/transfer），service 内置「业务意图→复式分录」科目规则，内部经 `LedgerService.Post` 落库。
2. **冲正端点** `POST /api/v1/vouchers/{voucher_no}/reverse`：蓝冲（默认，改状态+回滚）/ 红冲（退款，反向分录）。
3. **数据模型**：`acct_txn` 加 `voucher_no`（凭证号）+ `txn_status`（normal/reversed）。
4. **并发与原子性**：行锁事务 + lock ordering 防死锁；透支检查；事务原子（缺口 1）。
5. **Post 改造**：事务化 + 返回 txns（缺口 3）。
6. **测试**：service 单测（科目规则/透支/蓝红冲/防重复/原子性）+ handler 单测 + repo 并发集成 + e2e 冒烟。

### 2.2 B-3 非目标

- **冻结/解冻两阶段记账**——`frozen_amount` 字段暂不启用；两阶段在途资金模型是 payment/清算域的地道场景，留 payment 域或以后。
- **幂等键去重**（Idempotency-Key）——YAGNI，重复提交=重复记账，文档记录此简化；真实幂等留以后。
- **多日切日 biz_date 推进**——B-2。biz_date 暂取服务端当天（`time.Now()`），B-2 落地后改读 `sys_param.biz_date`。
- **定期账户记账**——聚焦活期（demand_account），定期（fixed）不开放写。
- **B-1/B-2/B-4 的任何改动**——B-3 只动 core-banking（service/repo/api/domain）+ core_db migration。
- **core-banking 只读 API 改动**——现有 GET 端点不动；`ListTxns` 可顺带返回 `txn_status`/`voucher_no`（见 §10 已知简化）。

---

## 3. 核心原则（延续 Spec A/B-1）

| 原则 | 在 B-3 的体现 |
|------|---------------|
| **金额 int64 分，禁 float** | 请求/响应 amount 用字符串（分），`domain.Money` 全链路；HTTP 边界 `ParseCents`/`String()` 转换。 |
| **复式记账只在 core** | 写接口只在 core-banking；记账/冲正内部经 `LedgerService.Post`。 |
| **复式不变量内部生效** | 借必等于贷由 service 生成的分录保证（业务分录天然平衡）+ Post 校验；不暴露原始 entries 给客户端。 |
| **依赖方向向内** | `api → service → repo → domain`；repo 不 import service；domain 零依赖。 |
| **缩影哲学** | 保留真实模式（凭证号/蓝红冲/透支/行锁），砍规模（3 种 action、单库活期、无幂等/无冻结两阶段）。 |
| **生成物自包含 / 自闭** | bank 工程离开 jiade 可跑；jiade 不联动 SCV/Porto。 |

---

## 4. 架构总览

### 4.1 写接口在分层中的位置

```
internal/corebanking/
├── domain/
│   ├── ledger.go        # Entry/Txn/GLBalance（已有）+ BookingKind/ReverseMode（新常量）
│   └── ...
├── repo/
│   ├── ledger_repo.go   # 改造：事务化（接受 tx）+ 返回生成 txn_id；加 UpdateTxnStatus/GetTxnsByVoucher
│   ├── txn_repo.go      # 加 LockAndReadBalances（ORDER BY FOR UPDATE）
│   └── tx.go            # 新：RunInTx 事务封装
├── service/
│   ├── ledger_service.go # Post 改造：事务化 + 返回 txns
│   ├── txn_service.go    # 新增 Record（记账）+ Reverse（冲正）
│   └── posting.go        # 新：业务意图→复式分录 的科目规则
└── api/
    ├── handlers.go      # 加 PostTxn / ReverseVoucher handler
    └── router.go        # 加 POST 路由
```

### 4.2 依赖装配（cmd/core-banking/main.go）

`Handlers` 新增 `LedgerSvc *service.LedgerService` + `TxnWriter`（记账/冲正依赖）。main.go 注入 `repo.NewLedgerRepo` + 共享 `*sql.DB`（事务封装需要）。

### 4.3 数据流

```
POST /api/v1/txns  {action:deposit, account_no, amount}
  → handler 校验请求体
  → txn_service.Record
      → db.BeginTx
      → 锁账户行 SELECT...ORDER BY FOR UPDATE
      → 读余额 + 校验(active/ccy/透支)
      → posting.BuildEntries(action, accounts, amount)  // 科目规则→复式分录
      → ledger_service.Post(tx, entries, bizDate, ccy)   // 返回 txns + voucher_no
      → Commit
  → 201 {voucher_no, biz_date, txns:[...]}

POST /api/v1/vouchers/{voucher_no}/reverse?mode=blue
  → txn_service.Reverse
      → db.BeginTx
      → GetTxnsByVoucher(原凭证流水)
      → 校验(存在/未重复冲正)
      → 蓝冲: UpdateTxnStatus(reversed) + ApplyBalanceDeltas(逆向delta) + UpsertGL(逆向)
        红冲: BuildReverseEntries → Post(tx, 反向分录)  // 新 voucher + ref_txn_id 关联
      → Commit
  → 200 {...}
```

---

## 5. 数据模型变更（core_db migration）

`db/migrations/core_db.sql` 的 `acct_txn` 表追加两列 + 索引。因 `migrate.Run` 按分号切 DDL 逐条执行（Spec A §13 已知限制：DDL 无嵌套分号），追加 `ALTER TABLE` 安全：

```sql
CREATE TABLE IF NOT EXISTS acct_txn ( ... );  -- 原表定义不变
CREATE INDEX IF NOT EXISTS idx_acct_txn_bizdate ON acct_txn(biz_date);
CREATE INDEX IF NOT EXISTS idx_acct_txn_acct ON acct_txn(account_no, biz_date);
-- ↓ B-3 新增
ALTER TABLE acct_txn ADD COLUMN IF NOT EXISTS voucher_no TEXT NOT NULL DEFAULT '';
ALTER TABLE acct_txn ADD COLUMN IF NOT EXISTS txn_status TEXT NOT NULL DEFAULT 'normal';
CREATE INDEX IF NOT EXISTS idx_acct_txn_voucher ON acct_txn(voucher_no);
```

- `voucher_no`：凭证号，一次记账（一笔业务）的所有复式分录共用。seed 既有流水 `voucher_no=''`（历史数据，查询兼容）。
- `txn_status`：`normal` / `reversed`。蓝冲置 `reversed`。

> **seed 兼容**：fixture 生成的历史 `acct_txn` 不带 voucher_no（DEFAULT ''）；B-3 写接口产生的新流水才带凭证号。读接口 `ListTxns` 对空 voucher_no 容错。

---

## 6. HTTP API 契约

### 6.1 记账 `POST /api/v1/txns`

**请求**（`action` 决定字段集）：

```jsonc
// deposit / withdraw
{ "action":"deposit", "account_no":"D0000000001", "amount":"100.00", "ccy":"CNY", "summary":"存入" }
// transfer
{ "action":"transfer", "from_account":"D0000000001", "to_account":"D0000000002",
  "amount":"50.00", "ccy":"CNY", "summary":"转账" }
```

- `amount`：字符串，**元.分格式**（如 `"100.00"`，2 位小数），由 `domain.ParseCents` 解析为分（注意：`ParseCents` 按元解析，`"10000"` 会被当作 10000 元而非 10000 分）。与只读 API 的 `balance`/`amount` 表示一致（`Money.String()` 输出 `"%d.%02d"`）。禁 float；小数超 2 位报错。
- `ccy`：必填，须与账户 `ccy` 一致。
- `summary`：可选，写入流水。

**响应 `201`**：

```jsonc
{ "voucher_no":"V20260716abcdef",
  "biz_date":"2026-07-16",
  "txns":[ {"txn_id":"T...","account_no":"D0000000001","dc_flag":"借","amount":"100.00","subject_code":"1001"},
           {"txn_id":"T...","account_no":"D0000000001","dc_flag":"贷","amount":"100.00","subject_code":"2011"} ] }
```

### 6.2 冲正 `POST /api/v1/vouchers/{voucher_no}/reverse?mode=blue|red`

- `mode` 默认 `blue`。

**响应 `200`**：

```jsonc
// 蓝冲：原流水置 reversed，余额/总账回滚，无新流水
{ "voucher_no":"V...", "mode":"blue", "status":"reversed", "txns":[] }
// 红冲：原流水不变，产生反向凭证 + 反向流水
{ "voucher_no":"V...", "mode":"red", "reversed_voucher_no":"V...", "txns":[...] }
```

---

## 7. 业务规则（service 内置，不暴露给客户端）

### 7.1 科目规则（`service/posting.go`）

对方科目常量 `CashSubject = "1001"`（库存现金）。账户科目从 `demand_account.subject_code` 查出（活期 = 2011）。

| action | service 构造的复式分录（走 Post） | 余额影响 |
|--------|-----------------------------------|----------|
| deposit（存入） | 借 `1001` / 贷 账户科目 | + |
| withdraw（支取） | 借 账户科目 / 贷 `1001` | − |
| transfer（转账） | 借 from 科目 / 贷 to 科目 | from − / to + |

三种 action 的分录均「借一贷一」，**天然平衡**（Post 的 ValidateBalance 必过）。

### 7.2 透支检查

withdraw / transfer 在事务内锁账户后读 `account_balance` 最新 biz_date 的 `available_balance`（= `balance − frozen_amount`，当前 frozen 恒 0）；`amount > available_balance` → 拒绝 `422`。deposit 不检查（只增不减）。

### 7.3 蓝/红冲

- **蓝冲（撤销/作废，默认）**：原凭证所有流水 `txn_status='reversed'`；用**逆向 delta** 复用 `ApplyBalanceDeltas` / `UpsertGL`（传翻转 dc_flag 后的 delta，符号自然反转）回滚余额/总账。**不新增流水**。适用柜员录错、当日撤销。
- **红冲（退款/冲销）**：构造反向分录（每条 dc_flag 翻转，金额不变）走 `Post`，生成**新 `voucher_no`** 的反向流水，`ref_txn_id` 指向原凭证代表流水；原流水 `txn_status` 不变。适用退款、跨日冲销。
- **防重复冲正**：原凭证若已有任何流水 `txn_status='reversed'`，或已有冲正流水（`ref_txn_id` 指向它）→ `409`。蓝冲与红冲互斥（一凭证只冲一次）。冲正本身不可再冲正（缩影简化）。

> **复式平衡保证**：记账分录天然平衡（Post 校验）；蓝冲逆向 delta 是已平衡原 delta 的镜像，天然平衡；红冲反向分录走 Post 校验。账务任何时刻借必等于贷。

---

## 8. 并发与事务原子性

### 8.1 事务封装（`repo/tx.go`）

新增 `RunInTx(db, fn func(*sql.Tx) error) error`：`BeginTx` → `fn` → `Commit`/`Rollback`。记账/冲正整体在 fn 内完成。

### 8.2 LedgerStore 事务化（缺口 1）

`Post` 原本三步用 `r.db`（连接池）非原子。改造为接受 `*sql.Tx`（或事务化的 store）：`InsertTxns` / `ApplyBalanceDeltas` / `UpsertGL` 在同一事务执行，任一失败整体回滚。

### 8.3 行锁 + lock ordering（防 AB-BA 死锁）

涉及 ≥2 账户的写（transfer、冲正）用统一加锁顺序：

```sql
SELECT account_no, balance, available_balance, frozen_amount
FROM account_balance
WHERE account_no IN ($1, $2)
ORDER BY account_no        -- 所有事务统一顺序加锁
FOR UPDATE;
```

- A→B 与 B→A 都先锁较小 `account_no`，加锁顺序全局一致，**消除 AB-BA 死锁**。
- 单账户（deposit/withdraw）锁一行，无死锁风险。
- Postgres 真发生死锁（理论兜底）返回 SQLSTATE `40P01` → service 透出 `409 Conflict`。

### 8.4 Post 返回 txns（缺口 3）

`Post` 签名由 `error` 改为返回生成的 `[]domain.Txn`（repo 生成 txn_id 后回填）；记账/冲正响应据此回传 `txn_id` / `voucher_no`。

---

## 9. 错误处理（对齐 Spec A §10）

| 场景 | HTTP 状态码 | 说明 |
|------|-------------|------|
| 请求体非法（缺字段 / action 非法 / amount≤0 / ccy 缺失或不一致） | 400 | handler 层校验 |
| 账户不存在 | 404 | demand/fixed 都查不到 |
| 账户非 active（frozen/closed）；重复冲正；死锁（40P01） | 409 | 业务冲突 |
| 透支（withdraw/transfer 余额不足） | 422 | 业务规则违反 |
| 内部/DB 错误 | 500 | 透出 errMap |

沿用现有 `writeJSON` / `errMap`（`{"error":"..."}`，中文信息）。事务内任何错误 → 回滚 + 对应状态码。

---

## 10. 测试策略（对齐 Spec A §9）

1. **service 纯逻辑单测**（fake store，扩展 `txn_service_test.go` / `posting_test.go`）：
   - 科目规则：deposit/withdraw/transfer 各自的分录构造正确（科目、dc_flag、金额）。
   - 透支：withdraw/transfer 余额不足拒绝；deposit 不检查。
   - 蓝冲：`txn_status` 置 reversed、逆向 delta 正确、余额/总账复原、无新流水。
   - 红冲：产生反向分录、新 voucher、ref_txn_id 关联、原流水不变。
   - 防重复冲正：已 reversed / 已有冲正流水 → 拒绝。
2. **事务原子性单测**：模拟 Post 中途失败（fake store 注入错误）→ 透支检查与写不分离、全回滚、余额未被触碰。
3. **handler 单测**（扩展 `handlers_test.go`，加 `postBody(path, body)` helper）：请求校验各错误码、响应契约（voucher_no/txns 字段）、冲正 mode 默认 blue。
4. **repo 集成**（真 pg，扩展 `repo/integration_test.go`）：
   - 行锁并发：两 goroutine 并发 A→B / B→A，均成功且不死锁、余额一致。
   - 事务回滚：注入失败后账户余额/流水无残留。
   - 逆向 delta 累加正确（蓝冲后余额复原）。
5. **e2e 冒烟**（Makefile/CI）：`记账(deposit)→查余额(+)→记账(withdraw)→查余额(−)→蓝冲(withdraw)→查余额(复原)`。

---

## 11. 验收标准

1. jiade 仓自身：`go build ./...` / `go test ./...` 全绿（不含 `templates/`）。
2. `templates/bank` 独立 module：拷到临时目录后 `go build ./... && go test ./...` 全绿（CI 单独步骤）。
3. `POST /api/v1/txns` 三种 action（deposit/withdraw/transfer）各能记账，响应含 `voucher_no` + 复式流水（借一贷一）。
4. withdraw/transfer 透支 → 422；账户 frozen/closed → 409；账户不存在 → 404。
5. 蓝冲：原流水 `txn_status=reversed`、余额/总账复原、无新流水；红冲：产生反向流水 + 新 voucher + ref_txn_id 关联。
6. 重复冲正（同 voucher 二次）→ 409。
7. 并发 A→B / B→A 不死锁（集成测试通过）。
8. `Post` 写入事务原子：中途失败全回滚（单测通过）。
9. e2e：记账→查余额→冲正→查余额链路正确。
10. 生成物自包含：未装 jiade（仅 docker+go）可 `docker compose up` + 跑写接口。

---

## 12. 已知简化（明确记录）

- **无幂等键**：重复提交 `POST /txns` 会重复记账（每次新 voucher_no）。缩影接受，文档记录；真实幂等留以后。
- **`frozen_amount` 未启用**：字段存在但 B-3 不动（恒 0，available=balance）。两阶段在途资金留 payment 域。
- **biz_date 取服务端当天**：`time.Now()`；B-2 后改读 `sys_param.biz_date`。
- **冲正不可再冲正**：蓝/红冲产生的凭证不能再被冲正（缩影简化，真实银行支持反冲）。
- **只聚焦活期**：定期账户不开放写。
- **seed 历史流水 voucher_no 为空**：读接口容错；蓝/红冲只针对 B-3 之后产生的凭证。

---

## 13. 必须延续的 Spec A/B-1 约束

- **自闭**：jiade 不联动 SCV/Porto，无 doctor。
- **生成物自包含**：bank 工程离开 jiade 可 `docker compose up` + `go run`。
- **金额 int64 分，禁 float**（HTTP 边界字符串 + ParseCents/String）。
- **复式记账只在 core**：写接口只在 core-banking，经 `LedgerService.Post`。
- **依赖方向向内** `api → service → repo → domain`；repo 不 import service；domain 零依赖。
- **module 边界**：`templates/bank` 是独立 module（`go.mod: module bank`），不参与 jiade build；改后须 `go generate ./internal/template` 重新打包 `templates.tar`（Makefile test/e2e 已依赖 generate）。
- **go 1.22**：bank module `go.mod` pin 1.22；本地验证 macOS 15 用 `CGO_ENABLED=0 GOTOOLCHAIN=go1.22.0`。
- **删除需确认**：`--reset`/`--force` 仍是破坏性操作的显式确认机制。

---

## 14. 后续衔接

- **B-2（多日切日引擎）**：落地后，记账 biz_date 改读 `sys_param.biz_date`（当前 B-3 取当天）；B-2 的逐日滚存与 B-3 写接口正交。
- **payment 域冻结/两阶段**：若未来 payment 需要在途资金模型，启用 `frozen_amount` + freeze/confirm 两阶段，与 B-3 的即时行锁记账并存（不同域不同模式）。

---

## 15. 开放问题

- 无待决问题。范围（记账+冲正、Post 内部化）、记账 action 模型与科目规则、透支检查（严格）、并发模型（行锁事务+lock ordering）、蓝/红冲语义（蓝冲改状态+逆向 delta / 红冲反向分录）、voucher_no + txn_status 字段、幂等不做、biz_date 取当天、seed 关系定位均已确认。
