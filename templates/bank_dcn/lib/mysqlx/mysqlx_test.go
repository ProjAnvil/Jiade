package mysqlx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestIsDuplicate(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"duplicate entry 1062", &mysql.MySQLError{Number: 1062, Message: "Duplicate entry '1' for key 'PRIMARY'"}, true},
		{"other mysql error", &mysql.MySQLError{Number: 1146, Message: "Table 'x.y' doesn't exist"}, false},
		{"wrapped duplicate", fmt.Errorf("insert account: %w", &mysql.MySQLError{Number: 1062}), true},
		{"wrapped non-mysql error", fmt.Errorf("insert account: %w", errors.New("boom")), false},
		{"plain error", errors.New("connection refused"), false},
		{"nil", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDuplicate(tt.err); got != tt.want {
				t.Fatalf("IsDuplicate(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestOpen(t *testing.T) {
	dsn := os.Getenv("DCN_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("DCN_TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}

	// Open retries for up to 60s, so probe reachability with a quick ping
	// first and only call Open once the database answers.
	probe, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Skipf("mysql not reachable (%v); skipping", err)
	}
	defer probe.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := probe.PingContext(ctx); err != nil {
		t.Skipf("mysql not reachable (%v); skipping", err)
	}

	db := Open(dsn)
	if db == nil {
		t.Fatal("Open returned nil")
	}
	defer db.Close()
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		t.Fatalf("ping after Open: %v", err)
	}
}
