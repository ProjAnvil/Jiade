package domains

import (
	"reflect"
	"testing"

	"bank/internal/fixtures"
)

func TestGenAccountRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	d1, f1 := GenAccountRows(cfg)
	d2, f2 := GenAccountRows(cfg)
	if !reflect.DeepEqual(d1, d2) || !reflect.DeepEqual(f1, f2) {
		t.Fatal("同 Config 两次 GenAccountRows 不一致（违反确定性）")
	}
	if len(d1) == 0 {
		t.Error("应生成活期账户")
	}
}

func TestGenBalanceRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	demand, _ := GenAccountRows(cfg)
	nos := []string{demand[0].AccountNo, demand[1].AccountNo}
	r1 := GenBalanceRows(cfg, nos)
	r2 := GenBalanceRows(cfg, nos)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("GenBalanceRows 不确定")
	}
}

func TestGenTxnRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	demand, _ := GenAccountRows(cfg)
	nos := make([]string, 10)
	for i := 0; i < 10; i++ {
		nos[i] = demand[i].AccountNo
	}
	r1 := GenTxnRows(cfg, nos)
	r2 := GenTxnRows(cfg, nos)
	if !reflect.DeepEqual(r1, r2) {
		t.Fatal("GenTxnRows 不确定")
	}
	// 每条流水应有确定性 txn_id（非随机 uuid）
	for _, tx := range r1 {
		if tx.TxnID == "" {
			t.Error("txn_id 不应为空")
		}
	}
}

func TestGenStaticData_FixedContent(t *testing.T) {
	d := GenStaticData(fixtures.DefaultConfig(fixtures.ScaleDev))
	if len(d.Ccys) != 4 || len(d.Branches) != 7 || len(d.Subjects) != 9 || len(d.Rates) != 4 {
		t.Errorf("静态数据量级不符: ccy=%d branch=%d subj=%d rate=%d",
			len(d.Ccys), len(d.Branches), len(d.Subjects), len(d.Rates))
	}
}
