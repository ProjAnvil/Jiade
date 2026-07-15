package service

import (
	"context"
	"database/sql"
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
