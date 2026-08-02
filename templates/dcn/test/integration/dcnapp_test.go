//go:build integration

package integration

import (
	"testing"
)

// 本单元转账：同单元两账户，余额差值精确。
func TestLocalTransferDelta(t *testing.T) {
	probe(t, unitBase("dcn01"))
	a, b, dcnA, _ := openPair(t, gnsBase, true)
	beforeA := balance(t, dcnA, a)
	code, raw := doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"fromId": a, "toId": b, "amount": "10.00",
	})
	if code != 200 {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	contains(t, raw, `"txId"`)
	if !decEq(balance(t, dcnA, a), decAdd(beforeA, "-10.00")) {
		t.Fatal("from balance delta mismatch")
	}
}
