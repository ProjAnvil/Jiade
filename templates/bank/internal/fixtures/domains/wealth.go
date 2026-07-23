package domains

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
	"bank/internal/wealth/domain"
)

// WealthStatic static table row collection (generated once; positions share rng seed+50 with products to avoid collision with daily +51).
type WealthStatic struct {
	Products []domain.WealthProduct
	Holdings []domain.WealthHolding
}

// GenWealthStatic generates financial products + initial positions (0-3 per client).
func GenWealthStatic(cfg fixtures.Config, custIDs []string, demandAccounts []string) WealthStatic {
	rng := fixtures.NewRNG(cfg.Seed + 50)
	products := make([]domain.WealthProduct, len(fixtures.WealthProducts))
	for i, p := range fixtures.WealthProducts {
		products[i] = domain.WealthProduct{
			ProductCode: p.Code, ProductName: p.Name, ProductType: p.Type, RiskLevel: p.Risk,
			ExpectedReturn: fmt.Sprintf("%.6f", p.ExpectedReturn),
			MinAmount:      domain.NewMoneyFromCents(int64(p.MinAmountYuan) * 100),
			TermDays:       p.TermDays,
			StartBizDate:   cfg.StartBizDate, EndBizDate: addDays(cfg.EndBizDate, 365),
			Status: "active",
		}
	}
	var holdings []domain.WealthHolding
	idx := 0
	for _, cid := range custIDs {
		n := rng.IntRange(0, 3)
		for j := 0; j < n; j++ {
			p := fixtures.WealthProducts[rng.IntRange(0, len(fixtures.WealthProducts)-1)]
			nav0 := 1 + rng.Float64()*0.25 // 4dp; spec §6.4 (the original implementation is 1+uniform(-0.05,0.2), the intentional alignment here is [1,1.25))
			amountYuan := maxInt(p.MinAmountYuan, rng.IntRange(0, 99999)*100)
			amount := domain.NewMoneyFromCents(int64(amountYuan) * 100)
			holdings = append(holdings, domain.WealthHolding{
				HoldingID: fmt.Sprintf("WP-HD-%07d", idx), CustID: cid,
				AccountNo: pickStr(rng, demandAccounts), ProductCode: p.Code, Ccy: "CNY",
				Share: fmt.Sprintf("%.4f", float64(amountYuan)/nav0), // non-amount decimal, 4dp text
				Cost:  amount, CurrentValue: amount,
				BizDate: cfg.StartBizDate,
			})
			idx++
		}
	}
	return WealthStatic{Products: products, Holdings: holdings}
}

// WriteWealthStatic writes wealth_product/wealth_holding idempotently (DELETE first and then INSERT).
func WriteWealthStatic(ctx context.Context, db *sql.DB, s WealthStatic) error {
	for _, t := range []string{"wealth_holding", "wealth_product"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, p := range s.Products {
		if _, err := db.ExecContext(ctx, `INSERT INTO wealth_product(product_code,product_name,product_type,risk_level,expected_return,min_amount,term_days,start_biz_date,end_biz_date,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			p.ProductCode, p.ProductName, p.ProductType, p.RiskLevel, p.ExpectedReturn,
			p.MinAmount.String(), p.TermDays, p.StartBizDate, p.EndBizDate, p.Status); err != nil {
			return err
		}
	}
	for _, h := range s.Holdings {
		if _, err := db.ExecContext(ctx, `INSERT INTO wealth_holding(holding_id,cust_id,account_no,product_code,ccy,share,cost,current_value,biz_date)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			h.HoldingID, h.CustID, h.AccountNo, h.ProductCode, h.Ccy, h.Share,
			h.Cost.String(), h.CurrentValue.String(), h.BizDate); err != nil {
			return err
		}
	}
	return nil
}

// RunWealth advances by bizDate: daily NAV walk (rollover) + three-factor order (daily independent rng) + daily interest (Q1-B).
// One pg.RunInTx per business day.
func RunWealth(ctx context.Context, db *sql.DB, cfg fixtures.Config, products []domain.WealthProduct, holdings []domain.WealthHolding, custIDs []string, demandAccounts []string) error {
	if len(products) == 0 {
		return fmt.Errorf("wealth: 无产品")
	}
	if len(custIDs) == 0 || len(demandAccounts) == 0 {
		return fmt.Errorf("wealth: 无客户或活期账户")
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("wealth: %w", err)
	}
	sf := fixtures.ScaleFactor(cfg.Scale)
	navState := make(map[string]float64, len(products))
	prodRet := make(map[string]float64, len(products))
	for _, p := range products {
		ret, _ := strconv.ParseFloat(p.ExpectedReturn, 64)
		prodRet[p.ProductCode] = ret
		navState[p.ProductCode] = 1 + ret/365 // Daily annualized slight increase as expected
	}
	// Position cost snapshot (for income; orders do not change positions)
	type holdingCost struct {
		costCents int64
		prodCode  string
	}
	costs := make([]holdingCost, len(holdings))
	for i, h := range holdings {
		costs[i] = holdingCost{costCents: h.Cost.Cents(), prodCode: h.ProductCode}
	}
	base := parseDate(cfg.StartBizDate)
	for _, d := range days {
		rng := fixtures.NewRNG(cfg.Seed + 51 + dayOrdinal(d, base)) // per-day rng (alignment reward)
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		// NAV travel (path dependency: navState rollover across days)
		navRows := make([]domain.WealthNav, 0, len(products))
		for _, p := range products {
			drift := navState[p.ProductCode] * (1 + (rng.Float64()*0.006 - 0.002))
			navState[p.ProductCode] = math.Round(math.Max(0.5, drift)*1e6) / 1e6
			navRows = append(navRows, domain.WealthNav{
				ProductCode: p.ProductCode, BizDate: dateStr,
				Nav:      fmt.Sprintf("%.6f", navState[p.ProductCode]),
				AccumNav: fmt.Sprintf("%.6f", navState[p.ProductCode]*1.1),
			})
		}
		// three factor order
		n := orderVolumeForDay(sf, factor)
		orders := make([]domain.WealthOrder, 0, n)
		for i := 0; i < n; i++ {
			// Order products are selected from the products parameter (same order and source as fixtures.WealthProducts, rng stream unchanged)
			p := products[rng.IntRange(0, len(products)-1)]
			amountYuan := maxInt(int(p.MinAmount.Cents()/100), rng.IntRange(0, 99999)*100) // Same position formula
			orders = append(orders, domain.WealthOrder{
				OrderID: fmt.Sprintf("WP-OD-%s-%05d", compact, i),
				BizDate: dateStr, CustID: pickStr(rng, custIDs), ProductCode: p.ProductCode,
				AccountNo: pickStr(rng, demandAccounts), OrderType: rng.Choice(fixtures.OrderTypes),
				Amount: domain.NewMoneyFromCents(int64(amountYuan) * 100),
				Share:  fmt.Sprintf("%.4f", float64(rng.IntRange(0, 999))), // Intentionally reserved quirk: share is independently random and not derived from amount/nav
				Nav:    fmt.Sprintf("%.6f", navState[p.ProductCode]),
				Status: "done",
			})
		}
		// Daily interest (Q1-B): cost per position × expected_return / 365, rounded to cents
		incomes := make([]domain.WealthIncome, 0, len(costs))
		for i, hc := range costs {
			incomes = append(incomes, domain.WealthIncome{
				IncomeID: fmt.Sprintf("WP-IC-%s-%05d", compact, i),
				BizDate:  dateStr, HoldingID: holdings[i].HoldingID,
				IncomeType: fixtures.IncomeTypes[0],
				Amount:     domain.NewMoneyFromCents(int64(math.Round(float64(hc.costCents) * prodRet[hc.prodCode] / 365))),
			})
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM wealth_nav WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 wealth_nav %s: %w", dateStr, err)
			}
			if err := bulkInsertWealthNavs(ctx, q, navRows); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM wealth_order WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 wealth_order %s: %w", dateStr, err)
			}
			if err := bulkInsertWealthOrders(ctx, q, orders); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM wealth_income WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 wealth_income %s: %w", dateStr, err)
			}
			return bulkInsertWealthIncomes(ctx, q, incomes)
		}); err != nil {
			return fmt.Errorf("wealth: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// orderVolumeForDay The number of financial management orders on the day (three-factor scaling; extracted as a function for order testing compared to weekends/working days).
func orderVolumeForDay(sf, factor float64) int {
	return maxInt(0, int(20*sf*factor))
}

// bulkInsertWealthNavs bulk inserts wealth_nav (4 columns).
func bulkInsertWealthNavs(ctx context.Context, q pg.DBTX, rows []domain.WealthNav) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 4
	const stmt = "INSERT INTO wealth_nav(product_code,biz_date,nav,accum_nav) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, r := range chunk {
			args = append(args, r.ProductCode, r.BizDate, r.Nav, r.AccumNav)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("wealth: 批量插 wealth_nav: %w", err)
		}
	}
	return nil
}

// bulkInsertWealthOrders bulk inserts wealth_order (10 columns).
func bulkInsertWealthOrders(ctx context.Context, q pg.DBTX, rows []domain.WealthOrder) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 10
	const stmt = "INSERT INTO wealth_order(order_id,biz_date,cust_id,product_code,account_no,order_type,amount,share,nav,status) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, o := range chunk {
			args = append(args, o.OrderID, o.BizDate, o.CustID, o.ProductCode, o.AccountNo,
				o.OrderType, o.Amount.String(), o.Share, o.Nav, o.Status)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("wealth: 批量插 wealth_order: %w", err)
		}
	}
	return nil
}

// bulkInsertWealthIncomes bulk inserts wealth_income (5 columns).
func bulkInsertWealthIncomes(ctx context.Context, q pg.DBTX, rows []domain.WealthIncome) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 5
	const stmt = "INSERT INTO wealth_income(income_id,biz_date,holding_id,income_type,amount) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, r := range chunk {
			args = append(args, r.IncomeID, r.BizDate, r.HoldingID, r.IncomeType, r.Amount.String())
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("wealth: 批量插 wealth_income: %w", err)
		}
	}
	return nil
}
