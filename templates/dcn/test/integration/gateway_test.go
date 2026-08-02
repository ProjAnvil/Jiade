//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// 网关接入层：前缀剥离 + /dcn/* LB 落任意单元仍完成本单元转账（透明转发）。
func TestGatewayRoutes(t *testing.T) {
	probe(t, gatewayBase)
	rid := fmt.Sprintf("itest-gw-%d", time.Now().UnixNano())
	id, dcn := openAccount(t, gatewayBase+"/gns", "集成测试", rid)
	code, raw := doJSON(t, "GET",
		fmt.Sprintf("%s/gns/locate?accountId=%d", gatewayBase, id), nil)
	if code != 200 {
		t.Fatalf("gateway locate: %d %s", code, raw)
	}
	contains(t, raw, `"dcn":"`+dcn+`"`)

	a, b, dcnA, _ := openPair(t, gatewayBase+"/gns", true)
	beforeA := balance(t, dcnA, a)
	code, raw = doJSON(t, "POST", gatewayBase+"/dcn/transfer", map[string]any{
		"fromId": a, "toId": b, "amount": "1.00",
	})
	if code != 200 {
		t.Fatalf("gateway transfer: %d %s", code, raw)
	}
	if !decEq(balance(t, dcnA, a), decAdd(beforeA, "-1.00")) {
		t.Fatal("gateway transfer balance delta mismatch")
	}
}
