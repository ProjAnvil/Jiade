//go:build integration

package integration

import (
	"fmt"
	"testing"
	"time"
)

// GNS external behavior: 404 before opening → open → locate hits → same requestId idempotently returns the same account.
func TestGNSOpenAndLocate(t *testing.T) {
	probe(t, gnsBase)
	code, _ := doJSON(t, "GET", gnsBase+"/locate?accountId=999999999", nil)
	if code != 404 {
		t.Fatalf("locate unknown = %d, want 404", code)
	}
	rid := fmt.Sprintf("itest-gns-%d", time.Now().UnixNano())
	id, dcn := openAccount(t, gnsBase, "itest-account", rid)
	if id <= 0 || dcn == "" {
		t.Fatalf("open = (%d, %s)", id, dcn)
	}
	code, raw := doJSON(t, "GET", fmt.Sprintf("%s/locate?accountId=%d", gnsBase, id), nil)
	if code != 200 {
		t.Fatalf("locate = %d", code)
	}
	contains(t, raw, `"dcn":"`+dcn+`"`)
	id2, _ := openAccount(t, gnsBase, "itest-account", rid)
	if id2 != id {
		t.Fatalf("idempotent open: %d != %d", id2, id)
	}
}
