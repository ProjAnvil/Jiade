//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// ADM 核对：转账事件汇总后 reconcile 最终 consistent。
func TestReconcileEventuallyConsistent(t *testing.T) {
	probe(t, admBase)
	a, b, dcnA, _ := openPair(t, gnsBase, true)
	if code, raw := doJSON(t, "POST", unitBase(dcnA)+"/transfer", map[string]any{
		"fromId": a, "toId": b, "amount": "1.00",
	}); code != 200 {
		t.Fatalf("transfer: %d %s", code, raw)
	}
	for i := 0; i < 10; i++ {
		code, raw := doJSON(t, "GET", admBase+"/reconcile", nil)
		if code == 200 {
			var v struct {
				Consistent bool `json:"consistent"`
			}
			if json.Unmarshal(raw, &v) == nil && v.Consistent {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("reconcile not consistent within 10s")
}
