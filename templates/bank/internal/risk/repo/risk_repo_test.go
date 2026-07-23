//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
	"bank/internal/risk/repo"
)

func setupRiskDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("risk_db")
	if err != nil {
		t.Skipf("无 risk_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestRiskRepo_ListRules(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	rules, err := repo.NewRiskRepo(db).ListRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Error("ListRules 应返回规则（seed 后有 5 条）")
	}
}

func TestRiskRepo_ListEventsAndBlacklists(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewRiskRepo(db)
	if _, err := r.ListEvents(ctx, "", "", "", "", 0, 10); err != nil {
		t.Fatalf("ListEvents 失败: %v", err)
	}
	if _, err := r.ListBlacklists(ctx, "", 0, 10); err != nil {
		t.Fatalf("ListBlacklists 失败: %v", err)
	}
}

func TestRiskRepo_GetEvent_NotFound(t *testing.T) {
	db := setupRiskDB(t)
	defer db.Close()
	_, err := repo.NewRiskRepo(db).GetEvent(context.Background(), "RS-EV-NOPE")
	if err == nil {
		t.Error("不存在的事件应返回错误")
	}
}
