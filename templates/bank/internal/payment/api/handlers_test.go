package api

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bank/internal/payment/domain"
	"bank/internal/payment/service"
)

type fakePayRepo struct {
	transfer *domain.Transfer
	merchant *domain.Merchant
	parties  *domain.TransferParty
}

func (f fakePayRepo) ListTransfers(context.Context, string, string, string, int, int) ([]domain.Transfer, error) {
	return nil, nil
}
func (f fakePayRepo) GetTransfer(context.Context, string) (domain.Transfer, error) {
	if f.transfer != nil {
		return *f.transfer, nil
	}
	return domain.Transfer{}, sql.ErrNoRows
}
func (f fakePayRepo) GetTransferParties(context.Context, string) (domain.TransferParty, error) {
	if f.parties != nil {
		return *f.parties, nil
	}
	return domain.TransferParty{}, sql.ErrNoRows
}
func (f fakePayRepo) GetMerchant(context.Context, string) (domain.Merchant, error) {
	if f.merchant != nil {
		return *f.merchant, nil
	}
	return domain.Merchant{}, sql.ErrNoRows
}

func get(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, _ := http.Get(srv.URL + path)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHealthz(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}

func TestGetTransfer_OK(t *testing.T) {
	h := &Handlers{Svc: service.NewPaymentService(fakePayRepo{transfer: &domain.Transfer{
		TxnID: "PT1", OutAccount: "D1", InAccount: "D2", Amount: domain.NewMoneyFromCents(100000),
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/payments/transfers/PT1")
	if code != 200 || !strings.Contains(body, `"amount":"1000.00"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}

func TestGetTransferParties(t *testing.T) {
	h := &Handlers{Svc: service.NewPaymentService(fakePayRepo{parties: &domain.TransferParty{
		TxnID: "PT1", OutAccount: "D1", OutCustName: "张伟", InAccount: "D2", InCustName: "李芳",
	}})}
	code, body := get(t, NewRouter(h), "/api/v1/payments/transfers/PT1/parties")
	if code != 200 || !strings.Contains(body, `"out_cust_name":"张伟"`) {
		t.Errorf("code=%d body=%s", code, body)
	}
}
