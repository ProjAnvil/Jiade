// Package pg provides PostgreSQL connection construction and transaction encapsulation.
package pg

import (
	"context"
	"database/sql"
)

// DBTX is the minimum interface that *sql.DB and *sql.Tx jointly satisfy, and is used to reuse the same set of repo methods inside and outside transactions.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// RunInTx executes fn within a DB transaction: Commit if fn returns nil, otherwise Rollback and reveal the original error.
// When panicking, rollback and then panic again. DB operations within fn should use the passed in q (i.e. *sql.Tx).
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
