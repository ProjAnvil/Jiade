//go:build integration

package pg_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
)

func TestRunInTx_RollsBackOnError(t *testing.T) {
	db, err := pg.Open("core_db")
	if err != nil {
		t.Skipf("无 core_db，跳过: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过: %v", err)
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
		return boom // 故意失败
	})
	if err != boom {
		t.Fatalf("应透出 boom, got %v", err)
	}
	var cnt int
	db.QueryRowContext(ctx, "SELECT count(*) FROM demand_account WHERE account_no='TX-D1'").Scan(&cnt)
	if cnt != 0 {
		t.Errorf("回滚后不应有残留行, got %d", cnt)
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

var _ error = errBoom{}

// 兼容 sql.Result 编译期检查
var _ = func() { var _ sql.Result = nil }
