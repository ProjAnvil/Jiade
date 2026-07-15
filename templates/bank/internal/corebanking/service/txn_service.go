package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/platform/pg"
)

// 记账/冲正错误。
var (
	ErrAccountNotFound     = fmt.Errorf("账户不存在")
	ErrAccountNotActive    = fmt.Errorf("账户非 active 状态")
	ErrInsufficientBalance = fmt.Errorf("余额不足")
	ErrCcyMismatch         = fmt.Errorf("币种不一致")
	ErrVoucherNotFound     = fmt.Errorf("凭证不存在")
	ErrAlreadyReversed     = fmt.Errorf("凭证已冲正")
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

// NewTxnService 构造记账服务：db 为事务边界；accounts 读取账户；ledger 过账；store 落库（含 SetTxnSummary）。
func NewTxnService(db *sql.DB, accounts AccountReader, ledger *LedgerService, store LedgerStore) *TxnService {
	return &TxnService{db: db, accounts: accounts, ledger: ledger, store: store}
}

// WithReader 注入只读 store（ListTxns/GetBalance 供 api 只读 handler 复用）。
func (s *TxnService) WithReader(read TxnStore) *TxnService { s.read = read; return s }

// RecordInput 记账请求。deposit/withdraw 用 AccountNo；transfer 用 FromAccount/ToAccount。
type RecordInput struct {
	Action                            domain.Action
	AccountNo, FromAccount, ToAccount string
	Amount                            domain.Money
	Ccy, Summary                      string
}

// Record 记账：业务意图 → 复式分录 → 事务内原子过账。
// 事务内：锁账户余额行(EnsureBalanceRow) → 校验(active/ccy/透支) → BuildEntries → Post → 落 summary。
// transfer 按 account_no 升序锁两账户（防 AB-BA 死锁）。
// 返回 domain.Booking（voucherNo + 复式流水）。
// 生产路径 db 非 nil → pg.RunInTx 包裹；db==nil（仅单元测试用 fake store）→ 直接以 q=nil 执行 fn。
func (s *TxnService) Record(ctx context.Context, in RecordInput) (domain.Booking, error) {
	bizDate := today()
	voucherNo := domain.NewVoucherNo(bizDate)
	var booking domain.Booking

	run := pg.RunInTx
	if s.db == nil {
		// 单测路径：fake store 忽略 q，直接执行 fn（事务原子性由集成测试覆盖）
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err := run(ctx, s.db, func(q pg.DBTX) error {
		// 解析主账户（单一获取，避免冗余二次查询）
		primaryNo := in.AccountNo
		if in.Action == domain.ActionTransfer {
			primaryNo = in.FromAccount
		}
		acct, err := s.accounts.GetDemand(ctx, primaryNo)
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

		// transfer 取对手账户（贷方）
		var counterparty *domain.DemandAccount
		if in.Action == domain.ActionTransfer {
			toAcct, err := s.accounts.GetDemand(ctx, in.ToAccount)
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

		// 锁账户余额行（按 account_no 升序，防 AB-BA 死锁）+ 透支检查
		// 透支：withdraw/transfer 的付款方（acct.AccountNo）余额不足则拒绝
		lockAccounts := lockedAccountList(in)
		for _, no := range lockAccounts {
			subject := acct.SubjectCode
			if in.Action == domain.ActionTransfer && no == in.ToAccount && counterparty != nil {
				subject = counterparty.SubjectCode
			}
			bal, err := s.store.EnsureBalanceRow(ctx, q, no, bizDate, subject)
			if err != nil {
				return err
			}
			// 仅对付款方做透支检查（acct.AccountNo）。deposit 不检查。
			if no == acct.AccountNo && (in.Action == domain.ActionWithdraw || in.Action == domain.ActionTransfer) {
				if in.Amount > bal.AvailableBalance {
					return ErrInsufficientBalance
				}
			}
		}

		// 过账（Post 不填 summary，summary 在 Post 后单独 UPDATE 落库）
		txns, err := s.ledger.Post(ctx, q, entries, bizDate, ccy, voucherNo, "")
		if err != nil {
			return err
		}
		// summary 落库（DB UPDATE，非内存赋值）
		if in.Summary != "" {
			if err := s.store.SetTxnSummary(ctx, q, voucherNo, in.Summary); err != nil {
				return err
			}
			for i := range txns {
				txns[i].Summary = in.Summary
			}
		}
		booking = domain.Booking{VoucherNo: voucherNo, BizDate: bizDate, Txns: txns}
		return nil
	})
	if err != nil {
		return domain.Booking{}, err
	}
	return booking, nil
}

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
//
// 并发安全（final review Important #1 修复）：
//   - 事务内先用 LockTxnsByVoucher（SELECT ... FOR UPDATE）锁本凭证所有流水行，串行化
//     对同一凭证的并发冲正。两个并发 Reverse 在此排队，第二个拿到锁时第一个已 commit。
//   - 蓝冲：先 HasReversal 拦截"红后蓝"（红冲不改 txn_status，normal 守卫拦不住）；
//     再 UpdateTxnStatus（WHERE 带 txn_status='normal' 守卫）拦"蓝蓝并发"。首笔把 normal→reversed
//     提交后，次笔 UPDATE 只剩 reversed 行可匹配 → RowsAffected=0 → sql.ErrNoRows →
//     ErrAlreadyReversed。即使忽略锁，这两层守卫也足以防止双回滚。
//   - 红冲：不改 txn_status（spec §7.3 红冲原流水 txn_status 不变）。改用 HasReversal(ref_txn_id)
//     判断首笔红冲反向分录是否已落库；首笔 Post 提交的 reverse entries 带 ref_txn_id，次笔拿到锁
//     后 HasReversal=true → ErrAlreadyReversed。
//
// 四种冲正顺序组合均有守卫（B-3 fix2 闭合"红后蓝"）：
//   - 蓝蓝：UpdateTxnStatus normal 守卫拒绝次笔 ✓
//   - 蓝红：`any TxnStatus==reversed` 检查拒绝 ✓
//   - 红红：HasReversal 拒绝 ✓
//   - 红蓝：蓝冲入口新增的 HasReversal 拒绝 ✓
//
// 生产路径 db 非 nil → pg.RunInTx 包裹；db==nil（单测）→ 直接以 q=nil 执行 fn。
func (s *TxnService) Reverse(ctx context.Context, voucherNo string, mode domain.ReverseMode) (ReverseResult, error) {
	bizDate := today()
	var res ReverseResult

	run := pg.RunInTx
	if s.db == nil {
		run = func(_ context.Context, _ *sql.DB, fn func(pg.DBTX) error) error { return fn(nil) }
	}
	err := run(ctx, s.db, func(q pg.DBTX) error {
		// 锁本凭证所有流水行（FOR UPDATE），串行化并发冲正。
		origs, err := s.store.LockTxnsByVoucher(ctx, q, voucherNo)
		if err != nil {
			return err
		}
		if len(origs) == 0 {
			return ErrVoucherNotFound
		}
		// 防重复：原凭证任一流水已 reversed → 拒绝（此检查在行锁内，对并发蓝冲有效）。
		for _, t := range origs {
			if t.TxnStatus == domain.TxnStatusReversed {
				return ErrAlreadyReversed
			}
		}
		ccy := origs[0].Ccy

		switch mode {
		case domain.ReverseBlue:
			// 红冲不改 txn_status（spec §7.3），normal 守卫拦不住"红后蓝"——用 HasReversal 兜底。
			// 蓝蓝并发由下面的 UpdateTxnStatus normal 守卫兜底；红蓝/红红由 HasReversal 兜底。
			if has, err := s.store.HasReversal(ctx, q, origs[0].TxnID); err != nil {
				return err
			} else if has {
				return ErrAlreadyReversed
			}
			// 原子 normal→reversed 守卫：并发蓝冲只有一个改到行。
			if err := s.store.UpdateTxnStatus(ctx, q, voucherNo, domain.TxnStatusReversed); err != nil {
				// RowsAffected=0（所有行已 reversed）→ sql.ErrNoRows → 视作已冲正。
				if errors.Is(err, sql.ErrNoRows) {
					return ErrAlreadyReversed
				}
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
			// 红冲不改 txn_status（spec §7.3）；用 HasReversal 判首笔红冲反向分录是否已落库。
			ref := origs[0].TxnID
			hasRev, err := s.store.HasReversal(ctx, q, ref)
			if err != nil {
				return err
			}
			if hasRev {
				return ErrAlreadyReversed
			}
			newVoucher := domain.NewVoucherNo(bizDate)
			entries := reverseEntries(origs)
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
// 注意 byAcct（净值）与 gl（发生额）符号语义不同：发生额回滚一律用 Sub。
func reverseRollback(txns []domain.Txn, bizDate string) ([]domain.BalanceDelta, domain.GLBalance) {
	byAcct := map[string]domain.Money{}
	subj := map[string]string{}
	glDC, glCC := domain.Money(0), domain.Money(0)
	for _, t := range txns {
		if t.DCFlag == domain.DCCredit {
			byAcct[t.AccountNo] = byAcct[t.AccountNo].Sub(t.Amount) // 原贷+ → 逆向-
			glCC = glCC.Sub(t.Amount)                                // 发生额回滚：减
		} else {
			byAcct[t.AccountNo] = byAcct[t.AccountNo].Add(t.Amount) // 原借- → 逆向+
			glDC = glDC.Sub(t.Amount)                                // 发生额回滚：减（不是 Add！）
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

// lockedAccountList 返回按 account_no 升序排列的待锁账户列表（防 AB-BA 死锁）。
// deposit/withdraw：仅 AccountNo。transfer：FromAccount+ToAccount。
func lockedAccountList(in RecordInput) []string {
	var list []string
	if in.Action == domain.ActionTransfer {
		list = []string{in.FromAccount, in.ToAccount}
	} else {
		list = []string{in.AccountNo}
	}
	sort.Strings(list)
	return list
}

func today() string { return time.Now().Format("2006-01-02") }

// --- 只读（保留 Spec A 原有能力，供 api handler 复用）---

// ListTxns 查流水（from/to 为 YYYY-MM-DD，空表示不限）。
func (s *TxnService) ListTxns(ctx context.Context, accountNo, from, to string) ([]domain.Txn, error) {
	if s.read == nil {
		return nil, fmt.Errorf("txn: 未注入只读 store")
	}
	return s.read.ListTxns(ctx, accountNo, from, to)
}

// GetBalance 取最新 biz_date 的账户余额。
func (s *TxnService) GetBalance(ctx context.Context, accountNo string) (domain.Balance, error) {
	if s.read == nil {
		return domain.Balance{}, fmt.Errorf("txn: 未注入只读 store")
	}
	return s.read.GetLatestBalance(ctx, accountNo)
}
