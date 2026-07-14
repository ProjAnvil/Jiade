// Package migrate 把 DDL 文本应用到已存在的数据库。
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Run 执行 DDL 文本（按分号切分语句，逐条执行）。
func Run(ctx context.Context, db *sql.DB, ddl string) error {
	for _, stmt := range SplitStatements(ddl) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: 执行失败 %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

// SplitStatements 按分号切分 SQL 语句（core_db.sql 无嵌套分号，安全）。
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
