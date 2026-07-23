// Package repo is the data access layer of reward service: this library SQL + customer HTTP API.
package repo

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"bank/internal/platform/serviceclient"
	"bank/internal/reward/domain"
)

// RewardRepo reward warehousing. This library only queries points_acct/coupon.
type RewardRepo struct {
	db       *sql.DB
	customer *serviceclient.Client
}

// NewRewardRepo constructs RewardRepo.
func NewRewardRepo(db *sql.DB) *RewardRepo {
	return &RewardRepo{
		db:       db,
		customer: serviceclient.New(getenv("CUSTOMER_URL", "http://localhost:18081")),
	}
}

// GetPointsAcct checks a single points account. There is no return wrapped sql.ErrNoRows.
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

// ListPointsAccts is filtered by member_level (no limit if empty) and paging. limit<=0 takes 50.
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

// ListCoupons Check customer coupons (if status is empty, no limit), pagination.
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

// GetProfile checks the library points account, and then calls customer to obtain the customer name/type.
func (r *RewardRepo) GetProfile(ctx context.Context, custID string) (domain.RewardProfile, error) {
	var p domain.RewardProfile
	var level sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT cust_id,points_balance,member_level FROM points_acct WHERE cust_id=$1`, custID).
		Scan(&p.CustID, &p.PointsBalance, &level)
	if err != nil {
		return domain.RewardProfile{}, fmt.Errorf("repo: 查积分档案 %s: %w", custID, err)
	}
	var customer struct {
		Name     string `json:"name"`
		CustType string `json:"cust_type"`
	}
	if err := r.customer.Get(ctx, "/api/v1/customers/"+serviceclient.EscapePath(custID), &customer); err != nil {
		return domain.RewardProfile{}, fmt.Errorf("repo: 从 customer 查客户 %s: %w", custID, err)
	}
	p.MemberLevel, p.CustName, p.CustType = level.String, customer.Name, customer.CustType
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

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
