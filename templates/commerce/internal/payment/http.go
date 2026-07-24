package payment

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"commerce/internal/platform/httpx"
)

const paymentBodyLimit = 1 << 20

// IntentView is the JSON projection of a payment intent for the query API.
type IntentView struct {
	PaymentIntentID string   `json:"payment_intent_id"`
	OrderID         string   `json:"order_id"`
	AmountMinor     int64    `json:"amount_minor"`
	Currency        string   `json:"currency"`
	Status          State    `json:"status"`
	Provider        string   `json:"provider"`
	RefundedMinor   int64    `json:"refunded_minor,omitempty"`
	Attempts        []Attempt `json:"attempts,omitempty"`
	Refunds         []Refund  `json:"refunds,omitempty"`
}

// NewHandler returns the payment HTTP API: intent lookup by order, and a
// simulated webhook replay endpoint. The simulated webhook is gated by the
// Service's deterministic provider so it cannot inject non-determinism.
func NewHandler(service *Service, store *PostgresStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/payments/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		orderID := r.PathValue("id")
		if strings.TrimSpace(orderID) == "" {
			writePaymentProblem(w, r, http.StatusBadRequest, "order_id_required")
			return
		}
		intent, found, err := store.GetIntentByOrder(r.Context(), orderID)
		if writePaymentStoreError(w, r, err) {
			return
		}
		if !found {
			writePaymentProblem(w, r, http.StatusNotFound, "payment_intent_not_found")
			return
		}
		view := IntentView{
			PaymentIntentID: intent.PaymentIntentID,
			OrderID:         intent.OrderID,
			AmountMinor:     intent.AmountMinor,
			Currency:        intent.Currency,
			Status:          intent.Status,
			Provider:        intent.Provider,
			RefundedMinor:   intent.RefundedMinor,
		}
		if attempts, err := store.ListAttempts(r.Context(), intent.PaymentIntentID); err == nil {
			view.Attempts = attempts
		}
		if refunds, err := store.ListRefunds(r.Context(), intent.PaymentIntentID); err == nil {
			view.Refunds = refunds
		}
		writePaymentJSON(w, http.StatusOK, view)
	})
	mux.HandleFunc("POST /api/v1/payments/webhooks", func(w http.ResponseWriter, r *http.Request) {
		if !requirePaymentJSON(w, r) {
			return
		}
		var body struct {
			OrderID     string `json:"order_id"`
			Currency    string `json:"currency"`
			AmountMinor int64  `json:"amount_minor"`
		}
		if err := decodePaymentJSON(w, r, &body); err != nil {
			writePaymentProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		result, err := service.CaptureOrder(r.Context(), CaptureCommand{
			OrderID:        body.OrderID,
			Currency:       body.Currency,
			AmountMinor:    body.AmountMinor,
			IdempotencyKey: placeIntentKey(body.OrderID),
		})
		if writePaymentServiceError(w, r, err) {
			return
		}
		status := http.StatusCreated
		if result.Replayed {
			status = http.StatusOK
		}
		writePaymentJSON(w, status, IntentView{
			PaymentIntentID: result.Intent.PaymentIntentID,
			OrderID:         result.Intent.OrderID,
			AmountMinor:     result.Intent.AmountMinor,
			Currency:        result.Intent.Currency,
			Status:          result.Intent.Status,
			Provider:        result.Intent.Provider,
		})
	})
	return mux
}

func requirePaymentJSON(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writePaymentProblem(w, r, http.StatusUnsupportedMediaType, "json_content_type_required")
		return false
	}
	return true
}

func decodePaymentJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, paymentBodyLimit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writePaymentJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writePaymentProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	httpx.WriteProblem(w, httpx.Problem{Status: status, Code: code, Instance: r.URL.Path})
}

func writePaymentStoreError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	writePaymentProblem(w, r, http.StatusInternalServerError, "internal_error")
	return true
}

func writePaymentServiceError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrInvalidCommand):
		writePaymentProblem(w, r, http.StatusUnprocessableEntity, "invalid_payment")
	case errors.Is(err, ErrIntentNotFound):
		writePaymentProblem(w, r, http.StatusNotFound, "payment_intent_not_found")
	case errors.Is(err, ErrRefundExceedsCaptured):
		writePaymentProblem(w, r, http.StatusUnprocessableEntity, "refund_exceeds_captured")
	default:
		writePaymentProblem(w, r, http.StatusInternalServerError, "internal_error")
	}
	return true
}
