// Package api is the transport layer of reward service: http handlers + chi router.
package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"bank/internal/reward/service"

	"github.com/go-chi/chi/v5"
)

// Handlers hold reward read-only services. Production is done by Svc proxy repo; for single testing
// service.NewRewardService(fakeRewardRepo) injection.
type Handlers struct {
	Svc *service.RewardService
}

// Healthz Survival Check.
func (h *Handlers) Healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GetPointsAcct checks a single points account. Does not exist and returns 404.
func (h *Handlers) GetPointsAcct(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	a, err := h.Svc.GetPointsAcct(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("积分账户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, pointsAcctResp{
		CustID: a.CustID, PointsBalance: a.PointsBalance, FrozenPoints: a.FrozenPoints,
		MemberLevel: a.MemberLevel, UpdateBizDate: a.UpdateBizDate,
	})
}

// ListPointsAccts filters and paging by member level (query: member_level/offset/limit).
func (h *Handlers) ListPointsAccts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	list, err := h.Svc.ListPointsAccts(r.Context(), q.Get("member_level"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]pointsAcctResp, 0, len(list))
	for _, a := range list {
		out = append(out, pointsAcctResp{
			CustID: a.CustID, PointsBalance: a.PointsBalance, MemberLevel: a.MemberLevel,
			UpdateBizDate: a.UpdateBizDate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"points_accounts": out})
}

// ListCoupons Check customer coupons (query: status/offset/limit).
func (h *Handlers) ListCoupons(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	id := chi.URLParam(r, "cust_id")
	list, err := h.Svc.ListCoupons(r.Context(), id, q.Get("status"), offset, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	out := make([]couponResp, 0, len(list))
	for _, c := range list {
		out = append(out, couponResp{
			CouponID: c.CouponID, CustID: c.CustID, CampaignID: c.CampaignID,
			FaceValue: c.FaceValue.String(), MinSpend: c.MinSpend.String(),
			Status: c.Status, IssueBizDate: c.IssueBizDate, ExpireDate: c.ExpireDate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"coupons": out})
}

// GetProfile checks the points file (cross-database federated JOIN).
func (h *Handlers) GetProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "cust_id")
	p, err := h.Svc.Profile(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errMap(errors.New("积分账户不存在")))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errMap(err))
		return
	}
	writeJSON(w, http.StatusOK, profileResp{
		CustID: p.CustID, PointsBalance: p.PointsBalance, MemberLevel: p.MemberLevel,
		CustName: p.CustName, CustType: p.CustType,
	})
}

// --- DTO ---

type pointsAcctResp struct {
	CustID        string `json:"cust_id"`
	PointsBalance int    `json:"points_balance"`
	FrozenPoints  int    `json:"frozen_points,omitempty"`
	MemberLevel   string `json:"member_level,omitempty"`
	UpdateBizDate string `json:"update_biz_date,omitempty"`
}

type couponResp struct {
	CouponID     string `json:"coupon_id"`
	CustID       string `json:"cust_id"`
	CampaignID   string `json:"campaign_id,omitempty"`
	FaceValue    string `json:"face_value"`
	MinSpend     string `json:"min_spend"`
	Status       string `json:"status,omitempty"`
	IssueBizDate string `json:"issue_biz_date,omitempty"`
	ExpireDate   string `json:"expire_date,omitempty"`
}

type profileResp struct {
	CustID        string `json:"cust_id"`
	PointsBalance int    `json:"points_balance"`
	MemberLevel   string `json:"member_level,omitempty"`
	CustName      string `json:"cust_name,omitempty"`
	CustType      string `json:"cust_type,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errMap(err error) map[string]string { return map[string]string{"error": err.Error()} }
