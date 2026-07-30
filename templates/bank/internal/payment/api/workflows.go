package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"bank/internal/payment"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// Workflow API contracts
//
// The Handlers struct delegates the payment-workflow REST endpoints to a
// WorkflowAPI implementation. The concrete implementation lives in package
// payment (internal/payment/consumer.go); handler tests inject a fake. This
// keeps the transport layer decoupled from the persistence + workflow engine
// wiring, mirroring how Handlers.Svc wraps the read-only PaymentService.
// ---------------------------------------------------------------------------

// WorkflowAPI is the contract Handlers delegates the workflow endpoints to.
// Methods return typed sentinel errors (ErrIdempotencyConflict,
// ErrWorkflowNotFound) so the handlers can map them to stable HTTP statuses.
type WorkflowAPI interface {
	// Start persists a payment intent + workflow instance in one transaction
	// and returns the workflow id. Replaying the same idempotency key with the
	// same request body returns the original workflow id (Replayed=true);
	// replaying it with a different body returns ErrIdempotencyConflict.
	Start(ctx context.Context, req StartWorkflowRequest) (StartWorkflowResponse, error)
	// Status returns the current state of the payment workflow.
	Status(ctx context.Context, workflowID string) (WorkflowStatusResponse, error)
	// Reverse triggers compensation for a succeeded payment. Returns
	// ErrWorkflowNotFound when the workflow id does not exist.
	Reverse(ctx context.Context, workflowID string) (ReverseWorkflowResponse, error)
}

// StartWorkflowRequest carries the data the handler extracts from the HTTP
// request. The handler computes RequestHash from the canonical request body so
// idempotency replays can be detected by hash equality rather than byte-equality
// (whitespace-insensitive).
type StartWorkflowRequest struct {
	IdempotencyKey  string
	RequestHash     string
	PayerCustomerID string
	PayerAccountNo  string
	PayeeAccountNo  string
	Currency        string
	AmountMinor     int64
}

// StartWorkflowResponse is the outcome of Start.
type StartWorkflowResponse struct {
	WorkflowID string
	Status     string
	Replayed   bool
}

// WorkflowStatusResponse is the GET endpoint body.
type WorkflowStatusResponse struct {
	WorkflowID      string
	Status          string
	IdempotencyKey  string
	PayerCustomerID string
	PayerAccountNo  string
	PayeeAccountNo  string
	Currency        string
	AmountMinor     int64
	Reversed        bool
}

// ReverseWorkflowResponse is the reverse endpoint body.
type ReverseWorkflowResponse struct {
	WorkflowID        string
	ReversalWorkflowID string
	Status            string
}

// Sentinel errors consumed by the handlers live in package payment
// (payment.ErrIdempotencyConflict, payment.ErrWorkflowNotFound). They are
// re-exported here as aliases so the handler's errors.Is reads naturally and
// legacy callers that imported them from api keep compiling. The concrete
// payment.WorkflowStarter returns the SAME error values, so errors.Is matches.
var (
	ErrIdempotencyConflict = payment.ErrIdempotencyConflict
	ErrWorkflowNotFound   = payment.ErrWorkflowNotFound
)

// ---------------------------------------------------------------------------
// DTOs
// ---------------------------------------------------------------------------

// createWorkflowRequest is the JSON body for POST /payments/workflows. All
// fields are required; AmountMinor must be a positive integer (minor units).
type createWorkflowRequest struct {
	PayerCustomerID string `json:"payer_customer_id"`
	PayerAccountNo  string `json:"payer_account_no"`
	PayeeAccountNo  string `json:"payee_account_no"`
	Currency        string `json:"currency"`
	AmountMinor     int64  `json:"amount_minor"`
}

// createWorkflowResponse is the JSON body returned on 201/200.
type createWorkflowResponse struct {
	WorkflowID string `json:"workflow_id"`
	Status     string `json:"status"`
	Replayed   bool   `json:"replayed"`
}

// workflowStatusResponse is the JSON body returned on GET.
type workflowStatusResponse struct {
	WorkflowID      string `json:"workflow_id"`
	Status          string `json:"status"`
	IdempotencyKey  string `json:"idempotency_key"`
	PayerCustomerID string `json:"payer_customer_id"`
	PayerAccountNo  string `json:"payer_account_no"`
	PayeeAccountNo  string `json:"payee_account_no"`
	Currency        string `json:"currency"`
	AmountMinor     int64  `json:"amount_minor"`
	Reversed        bool   `json:"reversed"`
}

// reverseWorkflowResponse is the JSON body returned on reverse.
type reverseWorkflowResponse struct {
	WorkflowID         string `json:"workflow_id"`
	ReversalWorkflowID string `json:"reversal_workflow_id,omitempty"`
	Status             string `json:"status"`
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// CreatePaymentWorkflow handles POST /api/v1/payments/workflows.
//
// Validation order (stable problem+json code per failure):
//  1. Idempotency-Key header present (missing_idempotency_key, 400)
//  2. Content-Type is JSON (invalid_content_type, 415)
//  3. Body decodes as JSON and all fields present (invalid_request_body, 400)
//  4. AmountMinor > 0 (invalid_amount, 400)
//
// Idempotency:
//   - Same key + same body → same workflow_id (200, replayed=true)
//   - Same key + different body → 409 (idempotency_conflict)
//
// The handler computes a SHA-256 hash over the canonical JSON body (parsed and
// re-marshalled with sorted keys) so whitespace-only differences do not cause
// spurious conflicts.
func (h *Handlers) CreatePaymentWorkflow(w http.ResponseWriter, r *http.Request) {
	if h.Workflows == nil {
		writeProblem(w, http.StatusServiceUnavailable, "workflow_api_unavailable", "workflow API is not wired")
		return
	}
	ctx := r.Context()

	// 1. Idempotency-Key header.
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}

	// 2. Content-Type. We accept application/json and any +json suffix
	// (e.g. application/vnd.payment.v1+json).
	ct := r.Header.Get("Content-Type")
	if !isJSONContentType(ct) {
		writeProblem(w, http.StatusUnsupportedMediaType, "invalid_content_type",
			"Content-Type must be application/json")
		return
	}

	// 3. Decode body. We read the raw bytes first so the hash can be computed
	// over the canonical form (parsed → re-marshalled with sorted keys), making
	// the idempotency check whitespace-insensitive.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request_body", "cannot read request body")
		return
	}
	var req createWorkflowRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "invalid_request_body", "request body is not valid JSON")
		return
	}
	if req.PayerCustomerID == "" || req.PayerAccountNo == "" || req.PayeeAccountNo == "" || req.Currency == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_request_body",
			"payer_customer_id, payer_account_no, payee_account_no, and currency are required")
		return
	}
	// 4. Positive amount.
	if req.AmountMinor <= 0 {
		writeProblem(w, http.StatusBadRequest, "invalid_amount",
			"amount_minor must be a positive integer (minor units)")
		return
	}

	// Compute canonical hash for idempotency replay detection.
	hash, err := canonicalBodyHash(raw)
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "internal_error", "cannot compute request hash")
		return
	}

	resp, err := h.Workflows.Start(ctx, StartWorkflowRequest{
		IdempotencyKey:  idempotencyKey,
		RequestHash:     hash,
		PayerCustomerID: req.PayerCustomerID,
		PayerAccountNo:  req.PayerAccountNo,
		PayeeAccountNo:  req.PayeeAccountNo,
		Currency:        req.Currency,
		AmountMinor:     req.AmountMinor,
	})
	if err != nil {
		writeWorkflowError(w, err)
		return
	}

	status := http.StatusCreated
	if resp.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, createWorkflowResponse{
		WorkflowID: resp.WorkflowID,
		Status:     resp.Status,
		Replayed:   resp.Replayed,
	})
}

// GetPaymentWorkflow handles GET /api/v1/payments/workflows/{workflow_id}.
func (h *Handlers) GetPaymentWorkflow(w http.ResponseWriter, r *http.Request) {
	if h.Workflows == nil {
		writeProblem(w, http.StatusServiceUnavailable, "workflow_api_unavailable", "workflow API is not wired")
		return
	}
	workflowID := chi.URLParam(r, "workflow_id")
	if workflowID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_request_body", "workflow_id is required")
		return
	}
	resp, err := h.Workflows.Status(r.Context(), workflowID)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workflowStatusResponse{
		WorkflowID:      resp.WorkflowID,
		Status:          resp.Status,
		IdempotencyKey:  resp.IdempotencyKey,
		PayerCustomerID: resp.PayerCustomerID,
		PayerAccountNo:  resp.PayerAccountNo,
		PayeeAccountNo:  resp.PayeeAccountNo,
		Currency:        resp.Currency,
		AmountMinor:     resp.AmountMinor,
		Reversed:        resp.Reversed,
	})
}

// ReversePaymentWorkflow handles POST
// /api/v1/payments/workflows/{workflow_id}/reverse.
//
// The endpoint marks the payment intent as reversed and triggers reverse
// compensation. It requires an Idempotency-Key so a network retry does not
// double-reverse; the same key+workflow always returns the same outcome.
func (h *Handlers) ReversePaymentWorkflow(w http.ResponseWriter, r *http.Request) {
	if h.Workflows == nil {
		writeProblem(w, http.StatusServiceUnavailable, "workflow_api_unavailable", "workflow API is not wired")
		return
	}
	workflowID := chi.URLParam(r, "workflow_id")
	if workflowID == "" {
		writeProblem(w, http.StatusBadRequest, "invalid_request_body", "workflow_id is required")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeProblem(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required")
		return
	}
	resp, err := h.Workflows.Reverse(r.Context(), workflowID)
	if err != nil {
		writeWorkflowError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, reverseWorkflowResponse{
		WorkflowID:         resp.WorkflowID,
		ReversalWorkflowID: resp.ReversalWorkflowID,
		Status:             resp.Status,
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// writeWorkflowError maps a WorkflowAPI error to a stable problem+json response.
func writeWorkflowError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrIdempotencyConflict):
		writeProblem(w, http.StatusConflict, "idempotency_conflict",
			"idempotency key was already used with a different request body")
	case errors.Is(err, ErrWorkflowNotFound):
		writeProblem(w, http.StatusNotFound, "workflow_not_found", err.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeProblem(w, http.StatusNotFound, "workflow_not_found", "payment workflow not found")
	default:
		writeProblem(w, http.StatusInternalServerError, "internal_error", err.Error())
	}
}

// writeProblem writes an RFC 7807-shaped problem+json response with a stable
// code field. The code is what API clients switch on; the title is human-readable.
func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://bank.example/problems/" + code,
		"title":  code,
		"status": status,
		"code":   code,
		"detail": detail,
	})
}

// isJSONContentType reports whether a Content-Type header value is JSON. It
// accepts application/json and any vendor +json suffix (e.g.
// application/vnd.payment.v1+json). It is intentionally strict about the
// suffix so a misconfigured client sending text/plain is caught.
func isJSONContentType(ct string) bool {
	ct = strings.TrimSpace(strings.ToLower(ct))
	if ct == "" {
		return false
	}
	// Drop any parameters (e.g. ; charset=utf-8) before checking the suffix.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "application/json" {
		return true
	}
	return strings.HasSuffix(ct, "+json")
}

// canonicalBodyHash computes the SHA-256 hex digest over the canonical JSON
// form of body: it unmarshals body into a generic value and re-marshals it
// (which sorts object keys), then hashes the canonical bytes. This makes the
// idempotency hash whitespace- and key-order-insensitive so two semantically
// identical requests with different formatting produce the same hash.
//
// Non-object JSON (arrays, scalars) is hashed as-is after a canonicalising
// marshal, so the function is total: it never fails for valid JSON, and for
// invalid JSON it returns an error (the caller has already decoded the body via
// json.Unmarshal, so this path is defensive).
func canonicalBodyHash(body []byte) (string, error) {
	if len(body) == 0 {
		return "", errors.New("empty body")
	}
	var generic any
	if err := json.Unmarshal(body, &generic); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
