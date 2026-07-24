package inventory

import (
	"os"
	"strings"
	"testing"
)

func TestPostgresReservationStoreOwnsTransactionLockMovementAndOutboxSQL(t *testing.T) {
	source, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	required := []string{
		"pg_advisory_xact_lock",
		"FOR UPDATE OF i",
		"FOR UPDATE OF r",
		"UPDATE inventory_level",
		"INSERT INTO reservation",
		"INSERT INTO stock_movement",
		"messaging.InsertOutbox",
		"tx.Commit(ctx)",
	}
	for _, fragment := range required {
		if !strings.Contains(text, fragment) {
			t.Errorf("store.go missing transactional gate %q", fragment)
		}
	}
}

func TestHandlersContainNoSQL(t *testing.T) {
	source, err := os.ReadFile("http.go")
	if err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(string(source))
	for _, keyword := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE "} {
		if strings.Contains(upper, keyword) {
			t.Errorf("inventory handler contains SQL keyword %q", keyword)
		}
	}
}
