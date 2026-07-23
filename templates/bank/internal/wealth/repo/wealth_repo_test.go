//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
	"bank/internal/wealth/repo"
)

func setupWealthDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("wealth_db")
	if err != nil {
		t.Skipf("无 wealth_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestWealthRepo_ListProducts(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	prods, err := repo.NewWealthRepo(db).ListProducts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prods) != 6 {
		t.Errorf("理财产品应 6 个, got %d", len(prods))
	}
}

func TestWealthRepo_Lists(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewWealthRepo(db)
	if _, err := r.ListNav(ctx, "", "", ""); err != nil {
		t.Fatalf("ListNav 失败: %v", err)
	}
	if _, err := r.ListHoldings(ctx, "", 0, 10); err != nil {
		t.Fatalf("ListHoldings 失败: %v", err)
	}
	if _, err := r.ListOrders(ctx, "", "", "", "", 0, 10); err != nil {
		t.Fatalf("ListOrders 失败: %v", err)
	}
	if _, err := r.ListIncomes(ctx, "", "", "", 0, 10); err != nil {
		t.Fatalf("ListIncomes 失败: %v", err)
	}
}

func TestWealthRepo_GetHoldingProfile_NotFound(t *testing.T) {
	db := setupWealthDB(t)
	defer db.Close()
	_, err := repo.NewWealthRepo(db).GetHoldingProfile(context.Background(), "WP-HD-NOPE")
	if err == nil {
		t.Error("不存在的持仓应返回错误")
	}
}
