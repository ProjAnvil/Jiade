package domains

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"

	"bank/internal/fixtures"
	"bank/internal/loan/domain"
	"bank/internal/platform/pg"
)

// LoanStatic static table row collection (generated once).
type LoanStatic struct {
	Products      []domain.LoanProduct
	Accounts      []domain.LoanAccount
	Disbursements []domain.LoanDisbursement
}

// GenLoanStatic generates loan products/IOUs/loans (rng seed+40; fixed 4 rows for products, IOU numbers derived from custIDs, zero Counts changes).
func GenLoanStatic(cfg fixtures.Config, custIDs []string) LoanStatic {
	rng := fixtures.NewRNG(cfg.Seed + 40)
	products := make([]domain.LoanProduct, len(fixtures.LoanProducts))
	for i, p := range fixtures.LoanProducts {
		products[i] = domain.LoanProduct{
			ProductCode: p.Code, ProductName: p.Name, LoanType: p.LoanType, RateType: "fixed",
			MinRate: fmt.Sprintf("%.6f", p.MinRate), MaxRate: fmt.Sprintf("%.6f", p.MaxRate),
			MaxTerm: p.MaxTerm, MaxAmount: domain.NewMoneyFromCents(int64(p.MaxAmountYuan) * 100),
			Status: "active",
		}
	}
	nLoans := maxInt(5, len(custIDs)/4)
	accounts := make([]domain.LoanAccount, 0, nLoans)
	disbs := make([]domain.LoanDisbursement, 0, nLoans)
	for i := 0; i < nLoans; i++ {
		cid := pickStr(rng, custIDs) // The order of drawing lots is fixed: cust → product → principal → rate → term → start → guarantee/branch → to_account
		p := fixtures.LoanProducts[rng.IntRange(0, len(fixtures.LoanProducts)-1)]
		// Formula: IntRange(0,99999)×(maxAmtYuan/100000) yuan, clamp to [10000, maxAmtYuan] (pure integer)
		principalYuan := rng.IntRange(0, 99999) * (p.MaxAmountYuan / 100000)
		principalYuan = maxInt(10000, minInt(principalYuan, p.MaxAmountYuan))
		principal := domain.NewMoneyFromCents(int64(principalYuan) * 100)
		rate := p.MinRate + rng.Float64()*(p.MaxRate-p.MinRate) // Ratio (not amount), float acceptable
		term := []int{12, 24}[rng.IntRange(0, 1)]
		if p.MaxTerm >= 36 {
			term = []int{12, 24, 36}[rng.IntRange(0, 2)]
		}
		start := fixtures.RandomDate(rng, cfg.StartBizDate, maxDateStr(cfg.StartBizDate, addMonths(cfg.EndBizDate, -1))) // short range guard
		loanNo := fmt.Sprintf("LN%07d", i)
		accounts = append(accounts, domain.LoanAccount{
			LoanNo: loanNo, CustID: cid, ProductCode: p.Code, Ccy: "CNY",
			Principal: principal, Balance: principal, Rate: fmt.Sprintf("%.6f", rate),
			StartBizDate: start, MatureDate: addMonths(start, term), TermMonths: term,
			Status: "disbursed", GuaranteeType: rng.Choice(fixtures.GuaranteeTypes),
			BranchCode: rng.Choice(fixtures.Branches),
		})
		disbs = append(disbs, domain.LoanDisbursement{
			DisbID: fmt.Sprintf("LN-DB-%07d", i), BizDate: start, LoanNo: loanNo,
			Amount: principal, ToAccount: fmt.Sprintf("D%010d", rng.IntRange(0, 9999999999)),
		})
	}
	return LoanStatic{Products: products, Accounts: accounts, Disbursements: disbs}
}

// WriteLoanStatic writes loan_product/loan_account/loan_disbursement idempotently (DELETE first and then INSERT).
func WriteLoanStatic(ctx context.Context, db *sql.DB, s LoanStatic) error {
	for _, t := range []string{"loan_disbursement", "loan_account", "loan_product"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, p := range s.Products {
		if _, err := db.ExecContext(ctx, `INSERT INTO loan_product(product_code,product_name,loan_type,rate_type,min_rate,max_rate,max_term,max_amount,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			p.ProductCode, p.ProductName, p.LoanType, p.RateType, p.MinRate, p.MaxRate,
			p.MaxTerm, p.MaxAmount.String(), p.Status); err != nil {
			return err
		}
	}
	for _, a := range s.Accounts {
		if _, err := db.ExecContext(ctx, `INSERT INTO loan_account(loan_no,cust_id,product_code,ccy,principal,balance,rate,start_biz_date,mature_date,term_months,status,guarantee_type,branch_code)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			a.LoanNo, a.CustID, a.ProductCode, a.Ccy, a.Principal.String(), a.Balance.String(), a.Rate,
			a.StartBizDate, a.MatureDate, a.TermMonths, a.Status, a.GuaranteeType, a.BranchCode); err != nil {
			return err
		}
	}
	for _, d := range s.Disbursements {
		if _, err := db.ExecContext(ctx, `INSERT INTO loan_disbursement(disb_id,biz_date,loan_no,amount,to_account,disb_ts)
			VALUES($1,$2,$3,$4,$5,CURRENT_TIMESTAMP)`,
			d.DisbID, d.BizDate, d.LoanNo, d.Amount.String(), d.ToAccount); err != nil {
			return err
		}
	}
	return nil
}

// loanState IOU rollover status (memory; balance rollover across days, path dependent).
type loanState struct {
	balance          domain.Money
	overdueDays      int
	overdueStart     string // Empty = not expired
	monthlyPrincipal domain.Money
	rateFloat        float64
}

// RunLoan advances according to bizDate: monthly repayment plan + overdue five-level classification slip + daily full balance snapshot.
// rng seed+41 single time (no daily randomization); one pg.RunInTx per business day. Full replay confirmed.
func RunLoan(ctx context.Context, db *sql.DB, cfg fixtures.Config, accounts []domain.LoanAccount) error {
	if len(accounts) == 0 {
		return fmt.Errorf("loan: 无借据")
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("loan: %w", err)
	}
	rng := fixtures.NewRNG(cfg.Seed + 41)
	state := make(map[string]*loanState, len(accounts))
	for _, a := range accounts {
		rateF, _ := strconv.ParseFloat(a.Rate, 64)
		state[a.LoanNo] = &loanState{
			balance:          a.Balance,
			monthlyPrincipal: domain.NewMoneyFromCents(roundDiv(a.Principal.Cents(), int64(a.TermMonths))),
			rateFloat:        rateF,
		}
	}
	// Overdue selection: ~8% (random_int(1,12)==1 caliber), overdue_start ∈ [start, max(start, end-2 months)]
	for _, a := range accounts {
		if rng.IntRange(1, 12) == 1 {
			state[a.LoanNo].overdueStart = fixtures.RandomDate(rng, cfg.StartBizDate, maxDateStr(cfg.StartBizDate, addMonths(cfg.EndBizDate, -2)))
		}
	}
	lastMonth := time.Month(0)
	for _, d := range days {
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		// At the beginning of the month (month rollover): Create a monthly repayment plan for balance>0 IOUs
		var repays []domain.LoanRepay
		monthStart := d.Month() != lastMonth
		if monthStart {
			lastMonth = d.Month()
			for i, a := range accounts {
				st := state[a.LoanNo]
				if st.balance <= 0 {
					continue
				}
				principalAmt := minMoney(st.monthlyPrincipal, st.balance)
				interestAmt := domain.NewMoneyFromCents(int64(math.Round(float64(st.balance.Cents()) * st.rateFloat / 12)))
				r := domain.LoanRepay{
					RepayID: fmt.Sprintf("LN-RP-%s-%05d", compact, i),
					BizDate: dateStr, LoanNo: a.LoanNo, DueDate: dateStr,
					PrincipalAmt: principalAmt, InterestAmt: interestAmt,
				}
				if st.overdueStart != "" && dateStr >= st.overdueStart {
					r.Status = "overdue" // Overdue payment will not be deducted and the balance will not be changed
				} else {
					st.balance = st.balance.Sub(principalAmt)
					if st.balance < 0 {
						st.balance = 0
					}
					r.PaidPrincipal, r.PaidInterest = principalAmt, interestAmt
					r.Status = "open"
				}
				repays = append(repays, r)
			}
		}
		// Cumulative number of overdue days (ISO date dictionary order is comparable)
		for _, a := range accounts {
			st := state[a.LoanNo]
			if st.overdueStart != "" && dateStr > st.overdueStart {
				st.overdueDays = int(dayOrdinal(d, parseDate(st.overdueStart)))
			}
		}
		// Full snapshot of the day + overdue slide
		var balances []domain.LoanBalance
		var overdues []domain.LoanOverdue
		for _, a := range accounts {
			st := state[a.LoanNo]
			if st.balance > 0 {
				balances = append(balances, domain.LoanBalance{
					LoanNo: a.LoanNo, BizDate: dateStr, PrincipalBalance: st.balance,
					InterestReceivable: domain.NewMoneyFromCents(int64(math.Round(float64(st.balance.Cents()) * st.rateFloat / 360))),
				})
			}
			if st.overdueDays > 0 && st.overdueStart != "" && dateStr > st.overdueStart {
				overdues = append(overdues, domain.LoanOverdue{
					OverdueID: fmt.Sprintf("LN-OD-%s-%s", compact, a.LoanNo),
					BizDate:   dateStr, LoanNo: a.LoanNo, OverdueDays: st.overdueDays,
					OverdueClass: overdueClass(st.overdueDays), OverdueAmount: st.balance,
				})
			}
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if monthStart {
				if _, err := q.ExecContext(ctx, "DELETE FROM loan_repay WHERE biz_date=$1", dateStr); err != nil {
					return fmt.Errorf("删当日 loan_repay %s: %w", dateStr, err)
				}
				if err := bulkInsertLoanRepays(ctx, q, repays); err != nil {
					return err
				}
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM loan_balance WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 loan_balance %s: %w", dateStr, err)
			}
			if err := bulkInsertLoanBalances(ctx, q, balances); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM loan_overdue WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 loan_overdue %s: %w", dateStr, err)
			}
			return bulkInsertLoanOverdues(ctx, q, overdues)
		}); err != nil {
			return fmt.Errorf("loan: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// bulkInsertLoanRepays bulk inserts loan_repay (9 columns).
func bulkInsertLoanRepays(ctx context.Context, q pg.DBTX, rows []domain.LoanRepay) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 9
	const stmt = "INSERT INTO loan_repay(repay_id,biz_date,loan_no,due_date,principal_amt,interest_amt,paid_principal,paid_interest,status) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, r := range chunk {
			args = append(args, r.RepayID, r.BizDate, r.LoanNo, r.DueDate,
				r.PrincipalAmt.String(), r.InterestAmt.String(),
				r.PaidPrincipal.String(), r.PaidInterest.String(), r.Status)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("loan: 批量插 loan_repay: %w", err)
		}
	}
	return nil
}

// bulkInsertLoanBalances bulk inserts loan_balance (4 columns).
func bulkInsertLoanBalances(ctx context.Context, q pg.DBTX, rows []domain.LoanBalance) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 4
	const stmt = "INSERT INTO loan_balance(loan_no,biz_date,principal_balance,interest_receivable) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, b := range chunk {
			args = append(args, b.LoanNo, b.BizDate, b.PrincipalBalance.String(), b.InterestReceivable.String())
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("loan: 批量插 loan_balance: %w", err)
		}
	}
	return nil
}

// bulkInsertLoanOverdues Bulk insert loan_overdue (6 columns).
func bulkInsertLoanOverdues(ctx context.Context, q pg.DBTX, rows []domain.LoanOverdue) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 6
	const stmt = "INSERT INTO loan_overdue(overdue_id,biz_date,loan_no,overdue_days,overdue_class,overdue_amount) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, o := range chunk {
			args = append(args, o.OverdueID, o.BizDate, o.LoanNo, o.OverdueDays, o.OverdueClass, o.OverdueAmount.String())
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("loan: 批量插 loan_overdue: %w", err)
		}
	}
	return nil
}

// overdueClass is divided into five levels according to the number of overdue days.
func overdueClass(days int) string {
	cls := "正常"
	for _, oc := range fixtures.OverdueClasses {
		if days >= oc.Days {
			cls = oc.Name
		}
	}
	return cls
}

// roundDiv Rounds integer division (a/b, a is non-negative).
func roundDiv(a, b int64) int64 {
	return (a + b/2) / b
}

// minMoney Smaller amount.
func minMoney(a, b domain.Money) domain.Money {
	if a < b {
		return a
	}
	return b
}

// maxDateStr returns the larger of the two YYYY-MM-DD (ISO lexicographic order).
func maxDateStr(a, b string) string {
	if b > a {
		return b
	}
	return a
}
