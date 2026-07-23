// Package migrate applies DDL text to an existing database.
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Run executes the DDL text (statements are separated by semicolons and executed one by one).
func Run(ctx context.Context, db *sql.DB, ddl string) error {
	for _, stmt := range SplitStatements(ddl) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: 执行失败 %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// SplitStatements splits SQL statements by semicolons (core_db.sql has no nested semicolons, safe).
func SplitStatements(sqlText string) []string {
	var out []string
	for _, s := range strings.Split(sqlText, ";") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
