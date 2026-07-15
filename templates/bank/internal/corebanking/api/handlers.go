// Package api 是 core-banking 传输层：http handlers + chi router。
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"

	"github.com/jackc/pgx/v5/pgconn"
)

// AccountReader 账户只读查询（repo.AccountRepo 实现）。
type AccountReader interface {
	GetDemand(ctx context.Context, accountNo string) (domain.DemandAccount, error)
	GetFixed(ctx context.Context, accountNo string) (domain.FixedAccount, error)
}

// LedgerReader 总账只读查询（repo.LedgerRepo 实现）。
type LedgerReader interface {
	GetGL(ctx context.Context, bizDate string) ([]domain.GLBalance, error)
}

// Handlers 持有所有只读依赖。
type Handlers struct {
	Accounts AccountReader
	TxnSvc   *service.TxnService
	Ledger   LedgerReader
}

// Healthz 存活检查。
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GetAccount 查账户（先活期，无则定期，都无则 404）。
func (h *Handlers) GetAccount(w http.ResponseWriter, r *http.Request) {
	no := chiURLParam(r, "account_no")
	ctx := r.Context()
	if d, err := h.Accounts.GetDemand(ctx, no); err == nil {
		writeJSON(w, http.StatusOK, accountResp{
			AccountNo: d.AccountNo, CustID: d.CustID, Type: "demand",
			Ccy: d.Ccy, Status: string(d.Status),
		})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	if f, err := h.Accounts.GetFixed(ctx, no); err == nil {
		writeJSON(w, http.StatusOK, accountResp{
			AccountNo: f.AccountNo, CustID: f.CustID, Type: "fixed", Ccy: f.Ccy,
			Status: string(f.Status), Principal: f.Principal.String(), Rate: f.Rate,
			Term: f.TermMonths, MatureDate: f.MatureDate,
		})
		return
	} else if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, errMap(errors.New("账户不存在")))
		return
	} else {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
}

// GetBalance 查最新 biz_date 余额。
func (h *Handlers) GetBalance(w http.ResponseWriter, r *http.Request) {
	no := chiURLParam(r, "account_no")
	b, err := h.TxnSvc.GetBalance(r.Context(), no)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("无余额记录")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	writeJSON(w, http.StatusOK, balanceResp{
		AccountNo: b.AccountNo, BizDate: b.BizDate, Balance: b.Balance.String(),
		Available: b.AvailableBalance.String(), Frozen: b.FrozenAmount.String(),
	})
}

// ListTxns 查流水（query: account_no/from/to）。
func (h *Handlers) ListTxns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	txns, err := h.TxnSvc.ListTxns(r.Context(), q.Get("account_no"), q.Get("from"), q.Get("to"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	out := make([]txnResp, 0, len(txns))
	for _, t := range txns {
		out = append(out, txnResp{
			TxnID: t.TxnID, BizDate: t.BizDate, AccountNo: t.AccountNo,
			DCFlag: string(t.DCFlag), Amount: t.Amount.String(), Summary: t.Summary,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"txns": out})
}

// GetLedger 查总账（query: biz_date）。
func (h *Handlers) GetLedger(w http.ResponseWriter, r *http.Request) {
	bizDate := r.URL.Query().Get("biz_date")
	if bizDate == "" {
		writeJSON(w, http.StatusBadRequest, errMap(errors.New("缺少 biz_date"))); return
	}
	gls, err := h.Ledger.GetGL(r.Context(), bizDate)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err)); return
	}
	out := make([]ledgerResp, 0, len(gls))
	for _, g := range gls {
		out = append(out, ledgerResp{
			SubjectCode: g.SubjectCode, BizDate: g.BizDate,
			DC: g.DCBalance.String(), CC: g.CCBalance.String(), Ccy: g.Ccy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ledger": out})
}

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

// statusFor 把 service 层错误映射到 HTTP 状态码。
func statusFor(err error) int {
	// Postgres 死锁（SQLSTATE 40P01）→ 409 Conflict（spec §8.3：客户端可安全重试）。
	// 必须先于 switch 检查：pgconn.PgError 是底层驱动错误，不会被 service 的哨兵 errors.Is 命中。
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		return http.StatusConflict
	}
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

type accountResp struct {
	AccountNo  string `json:"account_no"`
	CustID     string `json:"cust_id"`
	Type       string `json:"type"`
	Ccy        string `json:"ccy"`
	Status     string `json:"status"`
	Principal  string `json:"principal,omitempty"`
	Rate       string `json:"rate,omitempty"`
	Term       int    `json:"term_months,omitempty"`
	MatureDate string `json:"mature_date,omitempty"`
}

type balanceResp struct {
	AccountNo string `json:"account_no"`
	BizDate   string `json:"biz_date"`
	Balance   string `json:"balance"`
	Available string `json:"available"`
	Frozen    string `json:"frozen"`
}

type txnResp struct {
	TxnID     string `json:"txn_id"`
	BizDate   string `json:"biz_date"`
	AccountNo string `json:"account_no"`
	DCFlag    string `json:"dc_flag"`
	Amount    string `json:"amount"`
	Summary   string `json:"summary"`
}

type ledgerResp struct {
	SubjectCode string `json:"subject_code"`
	BizDate     string `json:"biz_date"`
	DC          string `json:"dc"`
	CC          string `json:"cc"`
	Ccy         string `json:"ccy"`
}

// --- B-3 write-path DTO ---

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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string {
	return map[string]string{"error": err.Error()}
}
