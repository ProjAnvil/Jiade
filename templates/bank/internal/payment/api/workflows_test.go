// Package api is the transport layer of the payment service.
//
// This file tests the payment-workflow REST endpoints (Task 7 brief Step 1):
//   - POST /api/v1/payments/workflows          (create)
//   - GET  /api/v1/payments/workflows/{id}     (status)
//   - POST /api/v1/payments/workflows/{id}/reverse (reverse a succeeded payment)
//
// The tests use a fake WorkflowAPI so they exercise only the transport
// contract: header validation, content-type, body validation, idempotency
// semantics (same key+body→same workflow id; same key+diff body→409), stable
// application/problem+json error codes, and routing.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeWorkflowAPI implements WorkflowAPI in memory for handler tests. It
// simulates the real starter's idempotency semantics: same key+hash returns the
// same workflow id (Replayed=true); same key+different hash returns
// ErrIdempotencyConflict; an unknown workflow id returns ErrWorkflowNotFound.
type fakeWorkflowAPI struct {
	mu         sync.Mutex
	stored     map[string]fakeStoredIntent
	uuidCount  int
	statusMap  map[string]WorkflowStatusResponse
	reverseMap map[string]ReverseWorkflowResponse
	statusErr  error
}

type fakeStoredIntent struct {
	requestHash string
	workflowID  string
}

func newFakeWorkflowAPI() *fakeWorkflowAPI {
	return &fakeWorkflowAPI{
		stored:     make(map[string]fakeStoredIntent),
		statusMap:  make(map[string]WorkflowStatusResponse),
		reverseMap: make(map[string]ReverseWorkflowResponse),
	}
}

func (f *fakeWorkflowAPI) Start(_ context.Context, req StartWorkflowRequest) (StartWorkflowResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.stored[req.IdempotencyKey]; ok {
		if existing.requestHash != req.RequestHash {
			return StartWorkflowResponse{}, ErrIdempotencyConflict
		}
		return StartWorkflowResponse{WorkflowID: existing.workflowID, Status: "pending", Replayed: true}, nil
	}
	f.uuidCount++
	wid := fmt.Sprintf("wf-%d", f.uuidCount)
	f.stored[req.IdempotencyKey] = fakeStoredIntent{requestHash: req.RequestHash, workflowID: wid}
	return StartWorkflowResponse{WorkflowID: wid, Status: "pending"}, nil
}

func (f *fakeWorkflowAPI) Status(_ context.Context, workflowID string) (WorkflowStatusResponse, error) {
	if f.statusErr != nil {
		return WorkflowStatusResponse{}, f.statusErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if resp, ok := f.statusMap[workflowID]; ok {
		return resp, nil
	}
	return WorkflowStatusResponse{}, ErrWorkflowNotFound
}

func (f *fakeWorkflowAPI) Reverse(_ context.Context, workflowID string) (ReverseWorkflowResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if resp, ok := f.reverseMap[workflowID]; ok {
		return resp, nil
	}
	return ReverseWorkflowResponse{}, ErrWorkflowNotFound
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// postWorkflowRaw sends body bytes to path with the given headers and returns
// (status, content-type, trimmed body). The caller controls the Content-Type
// header explicitly so tests can verify content-type validation.
func postWorkflowRaw(t *testing.T, h http.Handler, path string, body []byte, contentType string, headers map[string]string) (int, string, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Content-Type"), strings.TrimSpace(string(b))
}

func validCreateBody() map[string]any {
	return map[string]any{
		"payer_customer_id": "C-100",
		"payer_account_no":  "ACC-PAYER",
		"payee_account_no":  "ACC-PAYEE",
		"currency":          "CNY",
		"amount_minor":      50000,
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func decodeResponse(t *testing.T, body string) createWorkflowResponse {
	t.Helper()
	var resp createWorkflowResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode resp %q: %v", body, err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Test cases — brief Step 1
// ---------------------------------------------------------------------------

// TestCreateWorkflow_RequiresIdempotencyKey: missing Idempotency-Key → 400 with
// stable problem+json code.
func TestCreateWorkflow_RequiresIdempotencyKey(t *testing.T) {
	h := &Handlers{Workflows: newFakeWorkflowAPI()}
	code, ct, body := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		mustJSON(t, validCreateBody()), "application/json", nil)
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want %d (body=%s)", code, http.StatusBadRequest, body)
	}
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(body, `"code"`) || !strings.Contains(body, "missing_idempotency_key") {
		t.Errorf("body should carry stable code missing_idempotency_key: %s", body)
	}
}

// TestCreateWorkflow_RequiresJSONContentType: a non-JSON content type is rejected
// with 415 + problem+json.
func TestCreateWorkflow_RequiresJSONContentType(t *testing.T) {
	h := &Handlers{Workflows: newFakeWorkflowAPI()}
	code, ct, body := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		mustJSON(t, validCreateBody()), "text/plain", map[string]string{"Idempotency-Key": "idem-1"})
	if code != http.StatusUnsupportedMediaType {
		t.Errorf("code = %d, want %d (body=%s)", code, http.StatusUnsupportedMediaType, body)
	}
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(body, "invalid_content_type") {
		t.Errorf("body should carry stable code invalid_content_type: %s", body)
	}
}

// TestCreateWorkflow_RequiresPositiveAmount: amount_minor <= 0 → 400.
func TestCreateWorkflow_RequiresPositiveAmount(t *testing.T) {
	h := &Handlers{Workflows: newFakeWorkflowAPI()}
	body := validCreateBody()
	body["amount_minor"] = 0
	code, _, respBody := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		mustJSON(t, body), "application/json", map[string]string{"Idempotency-Key": "idem-1"})
	if code != http.StatusBadRequest {
		t.Errorf("code = %d, want %d (body=%s)", code, http.StatusBadRequest, respBody)
	}
	if !strings.Contains(respBody, "invalid_amount") {
		t.Errorf("body should carry stable code invalid_amount: %s", respBody)
	}
}

// TestCreateWorkflow_HappyPath: a valid request returns 201 with a workflow_id.
func TestCreateWorkflow_HappyPath(t *testing.T) {
	fake := newFakeWorkflowAPI()
	h := &Handlers{Workflows: fake}
	code, _, respBody := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		mustJSON(t, validCreateBody()), "application/json", map[string]string{"Idempotency-Key": "idem-1"})
	if code != http.StatusCreated {
		t.Fatalf("code = %d, want %d (body=%s)", code, http.StatusCreated, respBody)
	}
	resp := decodeResponse(t, respBody)
	if resp.WorkflowID != "wf-1" {
		t.Errorf("workflow_id = %q, want %q", resp.WorkflowID, "wf-1")
	}
}

// TestCreateWorkflow_SameKeySameBody_ReturnsSameWorkflowID: replaying the same
// idempotency key with the same body returns the SAME workflow id (200, not 201).
func TestCreateWorkflow_SameKeySameBody_ReturnsSameWorkflowID(t *testing.T) {
	fake := newFakeWorkflowAPI()
	h := &Handlers{Workflows: fake}
	body := mustJSON(t, validCreateBody())

	code1, _, body1 := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		body, "application/json", map[string]string{"Idempotency-Key": "idem-1"})
	if code1 != http.StatusCreated {
		t.Fatalf("first code = %d, want %d (body=%s)", code1, http.StatusCreated, body1)
	}
	resp1 := decodeResponse(t, body1)

	code2, _, body2 := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		body, "application/json", map[string]string{"Idempotency-Key": "idem-1"})
	if code2 != http.StatusOK {
		t.Errorf("second code = %d, want %d (body=%s)", code2, http.StatusOK, body2)
	}
	resp2 := decodeResponse(t, body2)
	if resp2.WorkflowID != resp1.WorkflowID {
		t.Errorf("second workflow_id = %q, want %q (same as first)", resp2.WorkflowID, resp1.WorkflowID)
	}
}

// TestCreateWorkflow_SameKeyDifferentBody_Returns409: replaying the same key
// with a DIFFERENT body returns 409 + problem+json.
func TestCreateWorkflow_SameKeyDifferentBody_Returns409(t *testing.T) {
	fake := newFakeWorkflowAPI()
	h := &Handlers{Workflows: fake}
	body1 := mustJSON(t, validCreateBody())

	code1, _, _ := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		body1, "application/json", map[string]string{"Idempotency-Key": "idem-1"})
	if code1 != http.StatusCreated {
		t.Fatalf("first code = %d, want %d", code1, http.StatusCreated)
	}

	body2 := validCreateBody()
	body2["amount_minor"] = 99999 // different body → different hash
	code2, ct, body2Str := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows",
		mustJSON(t, body2), "application/json", map[string]string{"Idempotency-Key": "idem-1"})
	if code2 != http.StatusConflict {
		t.Errorf("second code = %d, want %d (body=%s)", code2, http.StatusConflict, body2Str)
	}
	if !strings.Contains(ct, "application/problem+json") {
		t.Errorf("content-type = %q, want application/problem+json", ct)
	}
	if !strings.Contains(body2Str, "idempotency_conflict") {
		t.Errorf("body should carry stable code idempotency_conflict: %s", body2Str)
	}
}

// TestGetWorkflow_ReturnsStatus: a known workflow id returns 200 + status JSON.
func TestGetWorkflow_ReturnsStatus(t *testing.T) {
	fake := newFakeWorkflowAPI()
	fake.statusMap["wf-1"] = WorkflowStatusResponse{
		WorkflowID: "wf-1", Status: "succeeded",
		PayerAccountNo: "ACC-PAYER", PayeeAccountNo: "ACC-PAYEE",
		Currency: "CNY", AmountMinor: 50000,
	}
	h := &Handlers{Workflows: fake}
	srv := httptest.NewServer(NewRouter(h))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/payments/workflows/wf-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("code = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

// TestGetWorkflow_NotFound: an unknown workflow id returns 404 + problem+json.
func TestGetWorkflow_NotFound(t *testing.T) {
	h := &Handlers{Workflows: newFakeWorkflowAPI()}
	srv := httptest.NewServer(NewRouter(h))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/payments/workflows/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("code = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// TestReverseWorkflow_HappyPath: reversing a succeeded workflow returns 202.
func TestReverseWorkflow_HappyPath(t *testing.T) {
	fake := newFakeWorkflowAPI()
	fake.reverseMap["wf-1"] = ReverseWorkflowResponse{WorkflowID: "wf-1", Status: "compensating"}
	h := &Handlers{Workflows: fake}
	code, _, body := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows/wf-1/reverse",
		[]byte(`{}`), "application/json", map[string]string{"Idempotency-Key": "rev-1"})
	if code != http.StatusAccepted {
		t.Errorf("code = %d, want %d (body=%s)", code, http.StatusAccepted, body)
	}
}

// TestReverseWorkflow_UnknownID_Returns404: reversing an unknown workflow id → 404.
func TestReverseWorkflow_UnknownID_Returns404(t *testing.T) {
	h := &Handlers{Workflows: newFakeWorkflowAPI()}
	code, _, _ := postWorkflowRaw(t, NewRouter(h), "/api/v1/payments/workflows/nope/reverse",
		[]byte(`{}`), "application/json", map[string]string{"Idempotency-Key": "rev-1"})
	if code != http.StatusNotFound {
		t.Errorf("code = %d, want %d", code, http.StatusNotFound)
	}
}

// TestHealthz_StillWorks: adding the Workflows field MUST NOT break existing
// healthz route (no Workflows wiring required for legacy callers).
func TestHealthz_StillWorks(t *testing.T) {
	code, body := get(t, NewRouter(&Handlers{}), "/healthz")
	if code != 200 || !strings.Contains(body, "ok") {
		t.Errorf("healthz code=%d body=%s", code, body)
	}
}
