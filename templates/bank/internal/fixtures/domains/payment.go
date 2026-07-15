package domains

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/fixtures"
	"bank/internal/payment/domain"
)

// GenMerchants 生成 n 个商户（M%05d）。rng 偏移 +20。
func GenMerchants(cfg fixtures.Config, n int) []domain.Merchant {
	rng := fixtures.NewRNG(cfg.Seed + 20)
	out := make([]domain.Merchant, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Merchant{
			MerchantID:    fmt.Sprintf("M%05d", i),
			MerchantName:  rng.Choice(fixtures.Industries) + rng.Choice(fixtures.CustRegions) + "商戶",
			MCC:           rng.Choice(fixtures.MCCs),
			Region:        rng.Choice(fixtures.CustRegions),
			Status:        "active",
			CreateBizDate: fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
		}
	}
	return out
}

// GenTransfers 用 core 的活期账户生成转账。金额 int64 分（[1,999] → cents）。
// rng 偏移 +21。B-1 不做切日滚存，biz_date 散布在范围内（快照式）。
func GenTransfers(cfg fixtures.Config, demandNos []string, n int) []domain.Transfer {
	if len(demandNos) == 0 {
		return nil
	}
	rng := fixtures.NewRNG(cfg.Seed + 21)
	out := make([]domain.Transfer, n)
	for i := 0; i < n; i++ {
		amt := domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 1000) // [10.00, 9990.00]
		out[i] = domain.Transfer{
			TxnID:       fmt.Sprintf("PT%012d", i+1),
			BizDate:     fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
			OutAccount:  rng.Choice(demandNos),
			InAccount:   rng.Choice(demandNos),
			Amount:      amt,
			Ccy:         "CNY",
			Fee:         domain.NewMoneyFromCents(amt.Cents() / 1000), // 0.1% 手续费
			Channel:     rng.Choice(fixtures.Channels),
			CounterBank: rng.Choice(fixtures.CounterBanks),
			Status:      "success",
			Summary:     rng.Choice(fixtures.TransferSummaries),
		}
	}
	return out
}

// GenConsumptions 用 core 账户 + 商户生成消费。rng 偏移 +22。
func GenConsumptions(cfg fixtures.Config, demandNos []string, merchantIDs []string, n int) []domain.Consumption {
	if len(demandNos) == 0 {
		return nil
	}
	if len(merchantIDs) == 0 {
		merchantIDs = []string{"M00000"}
	}
	rng := fixtures.NewRNG(cfg.Seed + 22)
	out := make([]domain.Consumption, n)
	for i := 0; i < n; i++ {
		out[i] = domain.Consumption{
			TxnID:      fmt.Sprintf("PC%012d", i+1),
			BizDate:    fixtures.RandomDate(rng, cfg.StartBizDate, cfg.EndBizDate),
			AccountNo:  rng.Choice(demandNos),
			MerchantID: rng.Choice(merchantIDs),
			MCC:        rng.Choice(fixtures.MCCs),
			Amount:     domain.NewMoneyFromCents(int64(rng.IntRange(1, 999)) * 500), // [5.00, 4995.00]
			Ccy:        "CNY", Status: "success", Summary: "消费",
		}
	}
	return out
}

// WritePayments 幂等写 merchant + transfer_txn + consumption_txn（先 DELETE 后 INSERT）。
// 删除顺序保证消费 → 转账 → 商户（消费引用商户，最后删商户）。
func WritePayments(ctx context.Context, db *sql.DB,
	merchants []domain.Merchant, transfers []domain.Transfer, consumptions []domain.Consumption) error {
	for _, t := range []string{"consumption_txn", "transfer_txn", "merchant"} {
		if _, err := db.ExecContext(ctx, "DELETE FROM "+t); err != nil {
			return fmt.Errorf("清空 %s: %w", t, err)
		}
	}
	for _, m := range merchants {
		if _, err := db.ExecContext(ctx, `INSERT INTO merchant(merchant_id,merchant_name,mcc,region,status,create_biz_date)
			VALUES ($1,$2,$3,$4,$5,$6)`,
			m.MerchantID, m.MerchantName, m.MCC, m.Region, m.Status, m.CreateBizDate); err != nil {
			return err
		}
	}
	for _, t := range transfers {
		if _, err := db.ExecContext(ctx, `INSERT INTO transfer_txn
			(txn_id,biz_date,out_account,in_account,amount,ccy,fee,channel,counter_bank,status,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			t.TxnID, t.BizDate, t.OutAccount, t.InAccount, t.Amount.String(), t.Ccy, t.Fee.String(),
			t.Channel, t.CounterBank, t.Status, t.Summary); err != nil {
			return err
		}
	}
	for _, c := range consumptions {
		if _, err := db.ExecContext(ctx, `INSERT INTO consumption_txn
			(txn_id,biz_date,account_no,merchant_id,mcc,amount,ccy,status,summary)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			c.TxnID, c.BizDate, c.AccountNo, nullable(c.MerchantID), c.MCC, c.Amount.String(),
			c.Ccy, c.Status, c.Summary); err != nil {
			return err
		}
	}
	return nil
}
