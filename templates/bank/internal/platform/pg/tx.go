// Package pg 提供 PostgreSQL 连接构造与事务封装。
package pg

import (
	"context"
	"database/sql"
)

// DBTX 是 *sql.DB 与 *sql.Tx 共同满足的最小接口，用于在事务内外复用同一套 repo 方法。
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RunInTx 在一个 DB 事务内执行 fn：fn 返回 nil 则 Commit，否则 Rollback 并透出原错误。
// panic 时 Rollback 后重新 panic。fn 内的 DB 操作应使用传入的 q（即 *sql.Tx）。
func RunInTx(ctx context.Context, db *sql.DB, fn func(DBTX) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(tx)
}
