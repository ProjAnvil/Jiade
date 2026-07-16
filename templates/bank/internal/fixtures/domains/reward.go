package domains

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"bank/internal/fixtures"
	"bank/internal/platform/pg"
	"bank/internal/reward/domain"
)

// reward 会员等级（移植 Bossy reward.py LEVELS）。
var rewardLevels = []struct {
	Code, Name string
	Threshold  int
}{
	{"L1", "普通", 0}, {"L2", "银卡", 10000}, {"L3", "金卡", 50000},
	{"L4", "白金", 200000}, {"L5", "钻石", 1000000},
}

// coupon 面额/门槛候选（元 → 分）。
var (
	couponFaceCents = []int{1000, 2000, 5000, 10000}
	couponMinCents  = []int{0, 5000, 10000}
)

// RewardStatic 静态表行集合（一次性生成）。
type RewardStatic struct {
	MemberLevels []domain.MemberLevel
	Campaigns    []domain.Campaign
	PointsAccts  []domain.PointsAcct
}

// GenRewardStatic 生成 member_level/campaign/points_acct。rng 偏移 +30。
func GenRewardStatic(cfg fixtures.Config, custIDs []string) RewardStatic {
	rng := fixtures.NewRNG(cfg.Seed + 30)
	sf := fixtures.ScaleFactor(cfg.Scale)

	var levels []domain.MemberLevel
	for i, lv := range rewardLevels {
		ben, _ := json.Marshal(map[string]float64{"discount": 0.01 * float64(i)})
		levels = append(levels, domain.MemberLevel{
			LevelCode: lv.Code, LevelName: lv.Name,
			PointsThreshold: lv.Threshold, BenefitsJSON: string(ben),
		})
	}

	nCamp := maxInt(3, int(12*sf))
	campaigns := make([]domain.Campaign, 0, nCamp)
	for i := 0; i < nCamp; i++ {
		start := fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate)
		end := addDays(start, rng.IntRange(7, 60))
		campaigns = append(campaigns, domain.Campaign{
			CampaignID: fmt.Sprintf("CP%04d", i), Name: rng.Choice(fixtures.Industries) + rng.Choice(fixtures.CustRegions) + "活动",
			Type: rng.Choice(fixtures.CampaignTypes), StartBizDate: start, EndBizDate: end,
			Budget: domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 10000),
			Status: "active",
		})
	}

	accts := make([]domain.PointsAcct, len(custIDs))
	for i, cid := range custIDs {
		accts[i] = domain.PointsAcct{
			CustID: cid, PointsBalance: rng.IntRange(0, 5000), FrozenPoints: 0,
			MemberLevel: rng.Choice(fixtures.MemberLevelCodes), UpdateBizDate: cfg.StartBizDate,
		}
	}
	return RewardStatic{MemberLevels: levels, Campaigns: campaigns, PointsAccts: accts}
}

// WriteRewardStatic 幂等写 member_level/campaign/points_acct（先 DELETE 后 INSERT）。
func WriteRewardStatic(ctx context.Context, db *sql.DB, s RewardStatic) error {
	for _, t := range []string{"points_acct", "campaign", "member_level"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, lv := range s.MemberLevels {
		if _, err := db.ExecContext(ctx, `INSERT INTO member_level(level_code,level_name,points_threshold,benefits_json)
			VALUES($1,$2,$3,$4)`, lv.LevelCode, lv.LevelName, lv.PointsThreshold, lv.BenefitsJSON); err != nil {
			return err
		}
	}
	for _, c := range s.Campaigns {
		if _, err := db.ExecContext(ctx, `INSERT INTO campaign(campaign_id,name,type,start_biz_date,end_biz_date,budget,used_budget,status)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			c.CampaignID, c.Name, c.Type, c.StartBizDate, c.EndBizDate,
			c.Budget.String(), c.UsedBudget.String(), c.Status); err != nil {
			return err
		}
	}
	for _, a := range s.PointsAccts {
		if _, err := db.ExecContext(ctx, `INSERT INTO points_acct(cust_id,points_balance,frozen_points,member_level,update_biz_date)
			VALUES($1,$2,$3,$4,$5)`,
			a.CustID, a.PointsBalance, a.FrozenPoints, a.MemberLevel, a.UpdateBizDate); err != nil {
			return err
		}
	}
	return nil
}

// RunReward 按 bizDate 推进生成 points_txn + coupon（逐日三因子 + 每日独立 rng seed+31+ordinal）。
// balances 内存滚存自静态 points_acct 初始余额（redeem 不超余额，对齐 bossy）。逐日不回写 points_acct。
func RunReward(ctx context.Context, db *sql.DB, cfg fixtures.Config, accts []domain.PointsAcct, campaignIDs []string) error {
	if len(accts) == 0 {
		return fmt.Errorf("reward: 无积分账户")
	}
	if len(campaignIDs) == 0 {
		campaignIDs = []string{""}
	}
	days, err := bizDateRange(cfg.StartBizDate, cfg.EndBizDate)
	if err != nil {
		return fmt.Errorf("reward: %w", err)
	}
	sf := fixtures.ScaleFactor(cfg.Scale)
	balances := make(map[string]int, len(accts))
	custIDs := make([]string, len(accts))
	for i, a := range accts {
		balances[a.CustID] = a.PointsBalance
		custIDs[i] = a.CustID
	}
	base := parseDate(cfg.StartBizDate)
	for _, d := range days {
		factor := trendFactor(d) * seasonalFactor(d) * cyclicalFactor(d)
		n := maxInt(1, int(50*sf*factor))
		rng := fixtures.NewRNG(cfg.Seed + 31 + dayOrdinal(d, base))
		dateStr := d.Format("2006-01-02")
		compact := dateCompact(d)
		txns := make([]domain.PointsTxn, 0, n)
		var coupons []domain.Coupon
		for i := 0; i < n; i++ {
			cid := custIDs[rng.IntRange(0, len(custIDs)-1)]
			direction := rng.Choice(fixtures.PointDirections)
			pts := rng.IntRange(10, 500)
			if direction == "redeem" {
				pts = minInt(pts, balances[cid])
				balances[cid] = maxInt(0, balances[cid]-pts)
			} else {
				balances[cid] += pts
			}
			txns = append(txns, domain.PointsTxn{
				TxnID: fmt.Sprintf("RW-PT-%s-%05d", compact, i), CustID: cid, BizDate: dateStr,
				Points: pts, Direction: direction, SourceType: rng.Choice(fixtures.PointSources),
			})
			if rng.IntRange(1, 20) == 1 { // 5% 发券
				coupons = append(coupons, domain.Coupon{
					CouponID: fmt.Sprintf("RW-CP-%s-%05d", compact, i), CustID: cid,
					CampaignID: rng.Choice(campaignIDs),
					FaceValue:  domain.NewMoneyFromCents(int64(couponFaceCents[rng.IntRange(0, len(couponFaceCents)-1)])),
					MinSpend:   domain.NewMoneyFromCents(int64(couponMinCents[rng.IntRange(0, len(couponMinCents)-1)])),
					Status:     "issued", IssueBizDate: dateStr, ExpireDate: dateStr,
				})
			}
		}
		if err := pg.RunInTx(ctx, db, func(q pg.DBTX) error {
			if _, err := q.ExecContext(ctx, "DELETE FROM points_txn WHERE biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 points_txn %s: %w", dateStr, err)
			}
			if err := bulkInsertPointsTxns(ctx, q, txns); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM coupon WHERE issue_biz_date=$1", dateStr); err != nil {
				return fmt.Errorf("删当日 coupon %s: %w", dateStr, err)
			}
			return bulkInsertCoupons(ctx, q, coupons)
		}); err != nil {
			return fmt.Errorf("reward: 写 %s 失败: %w", dateStr, err)
		}
	}
	return nil
}

// bulkInsertPointsTxns 批量插 points_txn（8 列；ref_txn_id/summary nullable）。
func bulkInsertPointsTxns(ctx context.Context, q pg.DBTX, rows []domain.PointsTxn) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 8
	const stmt = "INSERT INTO points_txn(txn_id,cust_id,biz_date,points,direction,source_type,ref_txn_id,summary) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, t := range chunk {
			args = append(args, t.TxnID, t.CustID, t.BizDate, t.Points, t.Direction,
				nullable(t.SourceType), nullable(t.RefTxnID), nullable(t.Summary))
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("reward: 批量插 points_txn: %w", err)
		}
	}
	return nil
}

// bulkInsertCoupons 批量插 coupon（8 列）。空切片跳过。
func bulkInsertCoupons(ctx context.Context, q pg.DBTX, rows []domain.Coupon) error {
	if len(rows) == 0 {
		return nil
	}
	const cols = 8
	const stmt = "INSERT INTO coupon(coupon_id,cust_id,campaign_id,face_value,min_spend,status,issue_biz_date,expire_date) VALUES "
	for start := 0; start < len(rows); start += bizDateBatchSize {
		end := start + bizDateBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		args := make([]any, 0, len(chunk)*cols)
		for _, c := range chunk {
			args = append(args, c.CouponID, c.CustID, nullable(c.CampaignID), c.FaceValue.String(),
				c.MinSpend.String(), c.Status, c.IssueBizDate, c.ExpireDate)
		}
		if _, err := q.ExecContext(ctx, stmt+placeholders(len(chunk), cols), args...); err != nil {
			return fmt.Errorf("reward: 批量插 coupon: %w", err)
		}
	}
	return nil
}

// addDays 把 YYYY-MM-DD 加 n 天（n 可正可负）。
func addDays(dateStr string, n int) string {
	t, err := parseDate2(dateStr)
	if err != nil {
		return dateStr
	}
	return t.AddDate(0, 0, n).Format("2006-01-02")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
