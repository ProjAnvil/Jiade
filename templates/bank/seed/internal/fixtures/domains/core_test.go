package domains

import (
	"reflect"
	"testing"

	"bank/seed/internal/fixtures"
)

func TestGenAccountRows_Deterministic(t *testing.T) {
	cfg := fixtures.DefaultConfig(fixtures.ScaleDev)
	d1, f1 := GenAccountRows(cfg)
	d2, f2 := GenAccountRows(cfg)
	if !reflect.DeepEqual(d1, d2) || !reflect.DeepEqual(f1, f2) {
		t.Fatal("two GenAccountRows runs with same Config differ (determinism violated)")
	}
	if len(d1) == 0 {
		t.Error("should generate current accounts")
	}
}

func TestGenStaticData_FixedContent(t *testing.T) {
	d := GenStaticData(fixtures.DefaultConfig(fixtures.ScaleDev))
	if len(d.Ccys) != 4 || len(d.Branches) != 7 || len(d.Subjects) != 9 || len(d.Rates) != 4 {
		t.Errorf("static data scale mismatch: ccy=%d branch=%d subj=%d rate=%d",
			len(d.Ccys), len(d.Branches), len(d.Subjects), len(d.Rates))
	}
}
