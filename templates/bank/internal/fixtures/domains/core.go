// Package domains 是 fixture 的各业务域生成器。core = 核心账务。
package domains

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/corebanking/domain"
	"bank/internal/fixtures"
)

// ---- 静态主数据（确定性，移植自 bossy core.py）----

// StaticData 5 张主数据表的行集合。
type StaticData struct {
	SysParams [][2]string // {key, value}
	Ccys      [][4]string // {code, name, decimal_digits, status}
	Branches  [][5]string // {code, name, parent, region, level}
	Subjects  [][5]string // {code, name, dc_attr, level, parent}
	Rates     [][5]string // {rate_id, acct_type, ccy, rate_value, effective_date}
}

// GenStaticData 生成静态主数据（固定值 + cfg 的起始日）。
func GenStaticData(cfg fixtures.Config) StaticData {
	return StaticData{
		SysParams: [][2]string{
			{"biz_date", cfg.StartBizDate},
			{"prev_biz_date", cfg.StartBizDate},
			{"biz_status", "open"},
			{"last_cutover_ts", ""},
		},
		Ccys: [][4]string{
			{"CNY", "人民币", "2", "active"}, {"USD", "美元", "2", "active"},
			{"HKD", "港币", "2", "active"}, {"EUR", "欧元", "2", "active"},
		},
		Branches: [][5]string{
			{"HO", "总行", "", "华东", "1"}, {"SH", "上海分行", "HO", "华东", "2"},
			{"BJ", "北京分行", "HO", "华北", "2"}, {"GZ", "广州分行", "HO", "华南", "2"},
			{"CD", "成都分行", "HO", "西南", "2"}, {"SH-PD", "浦东支行", "SH", "华东", "3"},
			{"BJ-HD", "海淀支行", "BJ", "华北", "3"},
		},
		Subjects: [][5]string{
			{"1001", "库存现金", "借", "1", ""}, {"1002", "活期存款-资产", "借", "2", "1001"},
			{"2011", "活期存款", "贷", "2", ""}, {"2012", "定期存款", "贷", "2", ""},
			{"1301", "贷款", "借", "2", ""}, {"1311", "应收利息", "借", "2", ""},
			{"4001", "理财资金", "贷", "2", ""}, {"6011", "利息收入", "贷", "2", ""},
			{"6021", "手续费收入", "贷", "2", ""},
		},
		Rates: [][5]string{
			{"R-DMD-CNY", "demand", "CNY", "0.003000", cfg.StartBizDate},
			{"R-FIX3-CNY", "fixed_3m", "CNY", "0.012500", cfg.StartBizDate},
			{"R-FIX12-CNY", "fixed_12m", "CNY", "0.019000", cfg.StartBizDate},
			{"R-LOAN-CNY", "loan", "CNY", "0.043500", cfg.StartBizDate},
		},
	}
}

// GenAccountRows 生成活期/定期账户。cust_id 自生成（core-banking 自包含）。
func GenAccountRows(cfg fixtures.Config) ([]domain.DemandAccount, []domain.FixedAccount) {
	rng := fixtures.NewRNG(cfg.Seed + 1)
	tc := cfg.TargetCounts()
	nCustomers := tc.DemandAccounts / 2
	if nCustomers < 1 {
		nCustomers = 1
	}
	demand := make([]domain.DemandAccount, 0, tc.DemandAccounts)
	var fixed []domain.FixedAccount
	termRate := map[int]string{3: "0.012500", 6: "0.015000", 12: "0.019000"}
	terms := []int{3, 6, 12}
	for i := 0; i < tc.DemandAccounts; i++ {
		demand = append(demand, domain.DemandAccount{
			AccountNo: fmt.Sprintf("D%010d", i+1),
			CustID:    fmt.Sprintf("C%07d", (i%nCustomers)+1),
			Ccy:       "CNY", Status: domain.AccountStatusActive,
			OpenBizDate: cfg.StartBizDate, BranchCode: rng.Choice(fixtures.Branches),
			ProductCode: "DEMAND-CNY", SubjectCode: "2011",
		})
	}
	// 定期：约 DemandAccounts/4 个
	nFixed := tc.DemandAccounts / 4
	for i := 0; i < nFixed; i++ {
		term := terms[rng.IntRange(0, 2)]
		fixed = append(fixed, domain.FixedAccount{
			AccountNo:    fmt.Sprintf("F%010d", i+1),
			CustID:       fmt.Sprintf("C%07d", (i%nCustomers)+1),
			Ccy:          "CNY",
			Principal:    domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 10000),
			Rate:         termRate[term],
			TermMonths:   term,
			StartBizDate: cfg.StartBizDate,
			MatureDate:   addMonths(cfg.StartBizDate, term),
			Status:       domain.AccountStatusActive, SubjectCode: "2012",
		})
	}
	return demand, fixed
}

// GenBalanceRows / GenTxnRows 已由 Spec B-2 多日切日引擎（bizdate.go RunBizDate）取代，删除。

// ---- 落库 writer（幂等：先 DELETE 后 INSERT）----

// WriteStatic 写 5 张主数据表。
func WriteStatic(ctx context.Context, db *sql.DB, data StaticData) error {
	for _, t := range []string{"sys_param", "ccy", "branch", "chart_of_acct", "interest_rate"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, p := range data.SysParams {
		if _, err := db.ExecContext(ctx, "INSERT INTO sys_param(param_key,param_value) VALUES($1,$2)", p[0], p[1]); err != nil {
			return err
		}
	}
	for _, c := range data.Ccys {
		if _, err := db.ExecContext(ctx, "INSERT INTO ccy(ccy_code,ccy_name,decimal_digits,status) VALUES($1,$2,$3,$4)", c[0], c[1], c[2], c[3]); err != nil {
			return err
		}
	}
	for _, b := range data.Branches {
		var parent any
		if b[2] != "" {
			parent = b[2]
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO branch(branch_code,branch_name,parent_branch,region,level,status) VALUES($1,$2,$3,$4,$5,'active')", b[0], b[1], parent, b[3], b[4]); err != nil {
			return err
		}
	}
	for _, s := range data.Subjects {
		var parent any
		if s[4] != "" {
			parent = s[4]
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO chart_of_acct(subject_code,subject_name,dc_attr,level,parent_subject,status) VALUES($1,$2,$3,$4,$5,'active')", s[0], s[1], s[2], s[3], parent); err != nil {
			return err
		}
	}
	for _, r := range data.Rates {
		if _, err := db.ExecContext(ctx, "INSERT INTO interest_rate(rate_id,acct_type,ccy,rate_value,effective_biz_date,status) VALUES($1,$2,$3,$4,$5,'active')", r[0], r[1], r[2], r[3], r[4]); err != nil {
			return err
		}
	}
	return nil
}

// WriteAccounts 写活期/定期账户（先清后插）。
func WriteAccounts(ctx context.Context, db *sql.DB, demand []domain.DemandAccount, fixed []domain.FixedAccount) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM demand_account"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "DELETE FROM fixed_account"); err != nil {
		return err
	}
	for _, a := range demand {
		if _, err := db.ExecContext(ctx, `INSERT INTO demand_account
			(account_no,cust_id,ccy,acct_status,open_biz_date,branch_code,product_code,subject_code)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			a.AccountNo, a.CustID, a.Ccy, string(a.Status), a.OpenBizDate,
			a.BranchCode, a.ProductCode, a.SubjectCode); err != nil {
			return err
		}
	}
	for _, a := range fixed {
		if _, err := db.ExecContext(ctx, `INSERT INTO fixed_account
			(account_no,cust_id,ccy,principal,rate,term_months,start_biz_date,mature_date,acct_status,subject_code)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			a.AccountNo, a.CustID, a.Ccy, a.Principal.String(), a.Rate, a.TermMonths,
			a.StartBizDate, a.MatureDate, string(a.Status), a.SubjectCode); err != nil {
			return err
		}
	}
	return nil
}

// WriteBalances / WriteTxns 已由 Spec B-2 多日切日引擎（bizdate.go bulkInsert*）取代，删除。

// ---- helpers ----

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
