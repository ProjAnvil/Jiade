//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/payment/repo"
	"bank/internal/platform/pg"
)

func setupPayDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("pay_db")
	if err != nil {
		t.Skipf("无 pay_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestPaymentRepo_GetMerchant(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewPaymentRepo(db)
	db.ExecContext(ctx, "DELETE FROM merchant WHERE merchant_id='IT-M1'")
	db.ExecContext(ctx, `INSERT INTO merchant(merchant_id,merchant_name,mcc,region,status,create_biz_date)
		VALUES ('IT-M1','测试商户','5411','华东','active','2026-01-01')`)
	m, err := r.GetMerchant(ctx, "IT-M1")
	if err != nil {
		t.Fatal(err)
	}
	if m.MerchantName != "测试商户" {
		t.Errorf("got %+v", m)
	}
}

func TestPaymentRepo_TransfersAndParties(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewPaymentRepo(db)
	_, err := r.ListTransfers(ctx, "", "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListTransfers 失败: %v", err)
	}
	// 联邦 JOIN 不报错即可（依赖 seed 数据 + setup_fdw）
	_, err = r.GetTransferParties(ctx, "PT000000000001")
	if err != nil {
		t.Errorf("GetTransferParties FDW JOIN 失败（外部表未建？）: %v", err)
	}
}

func TestPaymentRepo_GetTransfer_NotFound(t *testing.T) {
	db := setupPayDB(t)
	defer db.Close()
	_, err := repo.NewPaymentRepo(db).GetTransfer(context.Background(), "NOPE")
	if err == nil {
		t.Error("应返回错误（不存在）")
	}
}
