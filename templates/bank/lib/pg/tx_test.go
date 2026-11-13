//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/lib/pg"
)

func TestRunInTx_RollsBackOnError(t *testing.T) {
	db, err := pg.Open("core_db")
	if err != nil {
		t.Skipf("no core_db, skipping: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres not ready, skipping: %v", err)
	}
	ctx := context.Background()
	db.ExecContext(ctx, "DELETE FROM demand_account WHERE account_no='TX-D1'")

	boom := errBoom{}
	err = pg.RunInTx(ctx, db, func(q pg.DBTX) error {
		if _, e := q.ExecContext(ctx, `INSERT INTO demand_account
			(account_no,cust_id,ccy,acct_status,open_biz_date,subject_code)
			VALUES ('TX-D1','C','CNY','active','2026-07-15','2011')`); e != nil {
			return e
		}
		return boom // Failure on purpose
	})
	if err != boom {
		t.Fatalf("should propagate boom, got %v", err)
	}
	var cnt int
	db.QueryRowContext(ctx, "SELECT count(*) FROM demand_account WHERE account_no='TX-D1'").Scan(&cnt)
	if cnt != 0 {
		t.Errorf("no rows should remain after rollback, got %d", cnt)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

var _ error = errBoom{}

// Compatible with sql.Result compile-time check
var _ = func() { var _ sql.Result = nil }
