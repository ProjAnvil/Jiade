//go:build integration

package main

import (
	"context"
	"testing"

	"bank/internal/platform/pg"
)

func TestEnsureDBs_CreatesAllThree(t *testing.T) {
	ctx := context.Background()
	// 先确保 admin 可连
	admin, err := pg.Open("postgres")
	if err != nil {
		t.Skipf("无 postgres 管理库连接，跳过: %v", err)
	}
	defer admin.Close()
	if err := admin.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make up）: %v", err)
	}
	if err := ensureDBs(ctx, true, []string{"core_db", "cust_db", "pay_db"}); err != nil {
		t.Fatalf("ensureDBs 失败: %v", err)
	}
	for _, name := range []string{"core_db", "cust_db", "pay_db"} {
		var exists bool
		if err := admin.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", name).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("库 %s 未被创建", name)
		}
	}
}
