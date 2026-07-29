package pg

import (
	"os"
	"strings"
	"testing"
)

func TestDSNUsesDedicatedHostForNamedDatabase(t *testing.T) {
	t.Setenv("DB_HOST", "shared-postgres")
	t.Setenv("DB_HOST_PAY_DB", "payment-db")
	t.Cleanup(func() { _ = os.Unsetenv("DB_HOST_PAY_DB") })

	if got := DSN("pay_db"); !strings.Contains(got, "@payment-db:") {
		t.Fatalf("DSN(pay_db)=%q, want payment-db host", got)
	}
}
