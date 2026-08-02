package dcnapp

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestInterestFor(t *testing.T) {
	rate := decimal.RequireFromString("0.0001")
	cases := []struct{ bal, want string }{
		// 注意：decimal.String() 裁剪尾零，0.10/10.00/1.00 输出为 0.1/10/1（数值语义不变）
		{"1000.00", "0.1"},
		{"100000.00", "10"},
		{"12.34", "0"},    // 0.001234 -> 0.00，跳过入账
		{"9999.995", "1"}, // 0.9999995 -> 1.00
		{"15", "0"},       // 0.0015 -> 0.00（不足半个取舍量子，舍去）
		{"25", "0"},       // 0.0025 -> 0.00
		{"35", "0"},       // 0.0035 -> 0.00
	}
	for _, c := range cases {
		got := InterestFor(decimal.RequireFromString(c.bal), rate)
		if got.String() != c.want {
			t.Errorf("InterestFor(%s) = %s, want %s", c.bal, got, c.want)
		}
	}
}

func TestInterestTxID(t *testing.T) {
	if got := interestTxID("2026-08-02", 1001); got != "interest-2026-08-02-1001" {
		t.Fatalf("interestTxID = %s", got)
	}
}
