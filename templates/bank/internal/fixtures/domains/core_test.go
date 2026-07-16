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

func TestGenStaticData_FixedContent(t *testing.T) {
	d := GenStaticData(fixtures.DefaultConfig(fixtures.ScaleDev))
	if len(d.Ccys) != 4 || len(d.Branches) != 7 || len(d.Subjects) != 9 || len(d.Rates) != 4 {
		t.Errorf("静态数据量级不符: ccy=%d branch=%d subj=%d rate=%d",
			len(d.Ccys), len(d.Branches), len(d.Subjects), len(d.Rates))
	}
}
