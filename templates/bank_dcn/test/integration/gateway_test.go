//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// Gateway access layer: prefix stripping + /dcn/* LB landing on any unit still completes a local transfer (transparent forwarding).
func TestGatewayRoutes(t *testing.T) {
	probe(t, gatewayBase)
	rid := fmt.Sprintf("itest-gw-%d", time.Now().UnixNano())
	id, dcn := openAccount(t, gatewayBase+"/gns", "itest-account", rid)
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
