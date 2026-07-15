package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenMerchants_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	a := GenMerchants(cfg, 10)
	b := GenMerchants(cfg, 10)
	if !reflect.DeepEqual(a, b) || len(a) != 10 {
		t.Error("GenMerchants 不确定或数量错")
	}
	if a[0].MerchantID != "M00000" {
		t.Errorf("首商户 id=%s", a[0].MerchantID)
	}
}

func TestGenTransfers_UsesCoreAccounts(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	accts := []string{"D0000000001", "D0000000002"}
	ts := GenTransfers(cfg, accts, 5)
	if len(ts) != 5 {
		t.Fatalf("len=%d", len(ts))
	}
	if ts[0].OutAccount != "D0000000001" && ts[0].OutAccount != "D0000000002" {
		t.Errorf("out_account 不在 core 账户集: %s", ts[0].OutAccount)
	}
	// 金额 int64 分（整数 * 100）
	if ts[0].Amount.Cents()%100 != 0 && ts[0].Amount.Cents() < 0 {
		t.Errorf("amount 异常: %d", ts[0].Amount.Cents())
	}
	// 确定性：相同配置 → 相同序列
	ts2 := GenTransfers(cfg, accts, 5)
	if !reflect.DeepEqual(ts, ts2) {
		t.Error("GenTransfers 不确定性")
	}
}

func TestGenConsumptions_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	accts := []string{"D0000000001", "D0000000002"}
	merchants := []string{"M00000", "M00001"}
	a := GenConsumptions(cfg, accts, merchants, 8)
	b := GenConsumptions(cfg, accts, merchants, 8)
	if !reflect.DeepEqual(a, b) || len(a) != 8 {
		t.Fatalf("GenConsumptions 不确定或数量错: len=%d", len(a))
	}
	if a[0].AccountNo != "D0000000001" && a[0].AccountNo != "D0000000002" {
		t.Errorf("account_no 不在 core 账户集: %s", a[0].AccountNo)
	}
	if a[0].MerchantID != "M00000" && a[0].MerchantID != "M00001" {
		t.Errorf("merchant_id 不在商户集: %s", a[0].MerchantID)
	}
	if a[0].TxnID != "PC000000000001" {
		t.Errorf("首笔 txn_id=%s", a[0].TxnID)
	}
}

func TestGenTransfers_EmptyDemandReturnsNil(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	if ts := GenTransfers(cfg, nil, 5); ts != nil {
		t.Errorf("空 demandNos 应返回 nil, got len=%d", len(ts))
	}
}

func TestGenConsumptions_EmptyDemandReturnsNil(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	if cs := GenConsumptions(cfg, nil, []string{"M00000"}, 5); cs != nil {
		t.Errorf("空 demandNos 应返回 nil, got len=%d", len(cs))
	}
}
