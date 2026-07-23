//go:build integration

package repo_test

import (
	"context"
	"database/sql"
	"testing"

	"bank/internal/platform/pg"
	"bank/internal/reward/repo"
)

func setupRewardDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := pg.Open("reward_db")
	if err != nil {
		t.Skipf("无 reward_db 连接，跳过: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres 未就绪，跳过（先 make seed）: %v", err)
	}
	return db
}

func TestRewardRepo_GetPointsAcct(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	ctx := context.Background()
	r := repo.NewRewardRepo(db)
	db.ExecContext(ctx, "DELETE FROM points_acct WHERE cust_id='IT-RC'")
	db.ExecContext(ctx, `INSERT INTO points_acct(cust_id,points_balance,frozen_points,member_level,update_biz_date)
		VALUES ('IT-RC',300,0,'L2','2026-01-01')`)
	got, err := r.GetPointsAcct(ctx, "IT-RC")
	if err != nil {
		t.Fatal(err)
	}
	if got.PointsBalance != 300 || got.MemberLevel != "L2" {
		t.Errorf("got %+v", got)
	}
}

func TestRewardRepo_ListPointsAccts(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	list, err := repo.NewRewardRepo(db).ListPointsAccts(context.Background(), "L2", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = list
}

func TestRewardRepo_ListCoupons(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	_, err := repo.NewRewardRepo(db).ListCoupons(context.Background(), "IT-RC", "", 0, 10)
	if err != nil {
		t.Fatalf("ListCoupons 失败: %v", err)
	}
}

func TestRewardRepo_GetProfile_ServiceCall(t *testing.T) {
	db := setupRewardDB(t)
	defer db.Close()
	// Cross-service aggregation only requires no error reporting (depending on seed data and customer services).
	_, err := repo.NewRewardRepo(db).GetProfile(context.Background(), "C0000001")
	if err != nil {
		t.Errorf("GetProfile 跨服务聚合失败（先 make up）: %v", err)
	}
}
