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

// WealthStatic 静态表行集合（一次性生成；持仓与产品共享 rng seed+50，避免与逐日 +51 碰撞）。
type WealthStatic struct {
	Products []domain.WealthProduct
	Holdings []domain.WealthHolding
}

// GenWealthStatic 生成理财产品 + 初始持仓（每客户 0-3 个，bossy 公式）。
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
			Status:         "active",
		}
	}
	var holdings []domain.WealthHolding
	idx := 0
	for _, cid := range custIDs {
		n := rng.IntRange(0, 3)
		for j := 0; j < n; j++ {
			p := fixtures.WealthProducts[rng.IntRange(0, len(fixtures.WealthProducts)-1)]
			nav0 := 1 + rng.Float64()*0.25 // 4dp；spec §6.4（bossy 为 1+uniform(-0.05,0.2)，Jiade 有意对齐为 [1,1.25)）
			amountYuan := maxInt(p.MinAmountYuan, rng.IntRange(0, 99999)*100)
			amount := domain.NewMoneyFromCents(int64(amountYuan) * 100)
			holdings = append(holdings, domain.WealthHolding{
				HoldingID: fmt.Sprintf("WP-HD-%07d", idx), CustID: cid,
				AccountNo: pickStr(rng, demandAccounts), ProductCode: p.Code, Ccy: "CNY",
				Share: fmt.Sprintf("%.4f", float64(amountYuan)/nav0), // 非金额小数，4dp 文本
				Cost:  amount, CurrentValue: amount,
				BizDate: cfg.StartBizDate,
			})
			idx++
		}
	}
	return WealthStatic{Products: products, Holdings: holdings}
}

// WriteWealthStatic 幂等写 wealth_product/wealth_holding（先 DELETE 后 INSERT）。
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

// RunWealth 按 bizDate 推进：每日 NAV 游走（滚存）+ 三因子订单（每日独立 rng）+ 每日利息（Q1-B）。
// 每业务日一个 pg.RunInTx。
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
		navState[p.ProductCode] = 1 + ret/365 // bossy：每日按预期年化微涨
	}
	// 持仓成本快照（供 income；订单不改持仓，对齐 bossy）
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
		rng := fixtures.NewRNG(cfg.Seed + 51 + dayOrdinal(d, base)) // per-day rng（对齐 reward）
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		// NAV 游走（路径依赖：navState 跨日滚存）
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
		// 三因子订单
		n := orderVolumeForDay(sf, factor)
		orders := make([]domain.WealthOrder, 0, n)
		for i := 0; i < n; i++ {
			p := fixtures.WealthProducts[rng.IntRange(0, len(fixtures.WealthProducts)-1)]
			amountYuan := maxInt(p.MinAmountYuan, rng.IntRange(0, 99999)*100) // 同持仓公式
			orders = append(orders, domain.WealthOrder{
				OrderID: fmt.Sprintf("WP-OD-%s-%05d", compact, i),
				BizDate: dateStr, CustID: pickStr(rng, custIDs), ProductCode: p.Code,
				AccountNo: pickStr(rng, demandAccounts), OrderType: rng.Choice(fixtures.OrderTypes),
				Amount: domain.NewMoneyFromCents(int64(amountYuan) * 100),
				Share:  fmt.Sprintf("%.4f", float64(rng.IntRange(0, 999))), // bossy quirk：share 独立随机，不由 amount/nav 推导
				Nav:    fmt.Sprintf("%.6f", navState[p.Code]),
				Status: "done",
			})
		}
		// 每日利息（Q1-B）：每持仓 cost × expected_return / 365，四舍五入到分
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

// orderVolumeForDay 当日理财订单笔数（三因子缩放；提取为函数供单测比周末/工作日）。
func orderVolumeForDay(sf, factor float64) int {
	return maxInt(0, int(20*sf*factor))
}

// bulkInsertWealthNavs 批量插 wealth_nav（4 列）。
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

// bulkInsertWealthOrders 批量插 wealth_order（10 列）。
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

// bulkInsertWealthIncomes 批量插 wealth_income（5 列）。
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
