// Package repo 是 reward 服务的仓储层：pgx raw SQL（本库 + 跨库 FDW JOIN）。
package repo

import (
	"context"
	"database/sql"
	"fmt"

	"bank/internal/reward/domain"
)

// RewardRepo reward 仓储。本库 points_acct/coupon 查询，并经 FDW 跨库 JOIN cust_db.cust_info。
type RewardRepo struct{ db *sql.DB }

// NewRewardRepo 构造 RewardRepo。
func NewRewardRepo(db *sql.DB) *RewardRepo { return &RewardRepo{db: db} }

// GetPointsAcct 查单个积分账户。不存在返回包装的 sql.ErrNoRows。
func (r *RewardRepo) GetPointsAcct(ctx context.Context, custID string) (domain.PointsAcct, error) {
	var a domain.PointsAcct
	var level sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT cust_id,points_balance,frozen_points,member_level,update_biz_date FROM points_acct WHERE cust_id=$1`,
		custID).Scan(&a.CustID, &a.PointsBalance, &a.FrozenPoints, &level, &a.UpdateBizDate)
	if err != nil {
		return domain.PointsAcct{}, fmt.Errorf("repo: 查积分账户 %s: %w", custID, err)
	}
	a.MemberLevel = level.String
	return a, nil
}

// ListPointsAccts 按 member_level 筛选（空则不限），分页。limit<=0 取 50。
func (r *RewardRepo) ListPointsAccts(ctx context.Context, memberLevel string, offset, limit int) ([]domain.PointsAcct, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT cust_id,points_balance,frozen_points,member_level,update_biz_date FROM points_acct
		WHERE ($1='' OR member_level=$1) ORDER BY cust_id LIMIT $2 OFFSET $3`, memberLevel, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列积分账户: %w", err)
	}
	defer rows.Close()
	var out []domain.PointsAcct
	for rows.Next() {
		var a domain.PointsAcct
		var level sql.NullString
		if err := rows.Scan(&a.CustID, &a.PointsBalance, &a.FrozenPoints, &level, &a.UpdateBizDate); err != nil {
			return nil, fmt.Errorf("repo: 列积分账户 scan: %w", err)
		}
		a.MemberLevel = level.String
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列积分账户: %w", err)
	}
	return out, nil
}

// ListCoupons 查客户优惠券（status 空则不限），分页。
func (r *RewardRepo) ListCoupons(ctx context.Context, custID, status string, offset, limit int) ([]domain.Coupon, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT coupon_id,cust_id,campaign_id,face_value,min_spend,status,issue_biz_date,expire_date
		FROM coupon WHERE ($1='' OR cust_id=$1) AND ($2='' OR status=$2)
		ORDER BY issue_biz_date DESC, coupon_id LIMIT $3 OFFSET $4`, custID, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("repo: 列优惠券: %w", err)
	}
	defer rows.Close()
	return scanCoupons(rows)
}

// GetProfile 跨库联邦：points_acct JOIN ext_cust_db_cust_info → 积分余额 + 会员等级 + 客户姓名/类型。
func (r *RewardRepo) GetProfile(ctx context.Context, custID string) (domain.RewardProfile, error) {
	var p domain.RewardProfile
	var level, name, ctype sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT pa.cust_id, pa.points_balance, pa.member_level, ci.name, ci.cust_type
		FROM points_acct pa
		LEFT JOIN ext_cust_db_cust_info ci ON pa.cust_id=ci.cust_id
		WHERE pa.cust_id=$1`, custID).
		Scan(&p.CustID, &p.PointsBalance, &level, &name, &ctype)
	if err != nil {
		return domain.RewardProfile{}, fmt.Errorf("repo: 联邦查积分档案 %s: %w", custID, err)
	}
	p.MemberLevel, p.CustName, p.CustType = level.String, name.String, ctype.String
	return p, nil
}

func scanCoupons(rows *sql.Rows) ([]domain.Coupon, error) {
	var out []domain.Coupon
	for rows.Next() {
		var c domain.Coupon
		var camp, face, minS, issue, exp sql.NullString
		if err := rows.Scan(&c.CouponID, &c.CustID, &camp, &face, &minS, &c.Status, &issue, &exp); err != nil {
			return nil, fmt.Errorf("repo: scan 优惠券: %w", err)
		}
		c.CampaignID, c.IssueBizDate, c.ExpireDate = camp.String, issue.String, exp.String
		fv, err := domain.ParseCents(face.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析优惠券金额: %w", err)
		}
		ms, err := domain.ParseCents(minS.String)
		if err != nil {
			return nil, fmt.Errorf("repo: 解析优惠券金额: %w", err)
		}
		c.FaceValue, c.MinSpend = fv, ms
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("repo: 列优惠券: %w", err)
	}
	return out, nil
}
