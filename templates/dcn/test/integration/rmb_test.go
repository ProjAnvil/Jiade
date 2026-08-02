//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// 跨单元转账经 RMB：COMMITTED + 两步骤 DONE + 同 txId 重放幂等（余额只变一次）。
func TestCrossUnitTransferAndIdempotentReplay(t *testing.T) {
	probe(t, rmbBase)
	a, b, dcnA, dcnB := openPair(t, gnsBase, false)
	txID := fmt.Sprintf("itest-rmb-%d", time.Now().UnixNano())
	beforeB := balance(t, dcnB, b)

	code, raw := doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"txId": txID, "fromId": a, "toId": b, "amount": "20.00",
	})
	if code != 200 {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	code, raw = doJSON(t, "GET", rmbBase+"/transactions/"+txID, nil)
	if code != 200 {
		t.Fatalf("get tx: %d", code)
	}
	contains(t, raw, `"status":"COMMITTED"`)
	contains(t, raw, `"status":"DONE"`)

	// 同 txId 重放：RMB 幂等返回，余额不二次变动
	code, _ = doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"txId": txID, "fromId": a, "toId": b, "amount": "20.00",
	})
	if code != 200 {
		t.Fatal("replay should still succeed (idempotent)")
	}
	time.Sleep(time.Second)
	if !decEq(balance(t, dcnB, b), decAdd(beforeB, "20.00")) {
		t.Fatal("replay must not double-credit")
	}
}
