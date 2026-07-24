package order

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"commerce/internal/platform/httpx"
)

const orderBodyLimit = 1 << 20

func NewHandler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/carts", func(w http.ResponseWriter, r *http.Request) {
		if !requireOrderJSON(w, r) {
			return
		}
		var body struct {
			CustomerID string `json:"customer_id"`
			Currency   string `json:"currency"`
		}
		if err := decodeOrderJSON(w, r, &body); err != nil {
			writeOrderProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		cart, err := service.CreateCart(r.Context(), CreateCartCommand{
			CustomerID: body.CustomerID, Currency: body.Currency,
		})
		if writeOrderServiceError(w, r, err) {
			return
		}
		writeCart(w, http.StatusCreated, cart)
	})
	mux.HandleFunc("GET /api/v1/carts/{id}", func(w http.ResponseWriter, r *http.Request) {
		cart, err := service.GetCart(r.Context(), r.PathValue("id"))
		if writeOrderServiceError(w, r, err) {
			return
		}
		writeCart(w, http.StatusOK, cart)
	})
	mux.HandleFunc("POST /api/v1/carts/{id}/items", func(w http.ResponseWriter, r *http.Request) {
		mutateCartHTTP(w, r, service, CartAddLine, "")
	})
	mux.HandleFunc("PATCH /api/v1/carts/{id}/items/{sku}", func(w http.ResponseWriter, r *http.Request) {
		mutateCartHTTP(w, r, service, CartChangeLine, r.PathValue("sku"))
	})
	mux.HandleFunc("DELETE /api/v1/carts/{id}/items/{sku}", func(w http.ResponseWriter, r *http.Request) {
		mutateCartHTTP(w, r, service, CartRemoveLine, r.PathValue("sku"))
	})
	mux.HandleFunc("POST /api/v1/checkouts", func(w http.ResponseWriter, r *http.Request) {
		if !requireOrderJSON(w, r) {
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeOrderProblem(w, r, http.StatusBadRequest, "idempotency_key_required")
			return
		}
		var body struct {
			CartID    string `json:"cart_id"`
			AddressID string `json:"address_id"`
		}
		if err := decodeOrderJSON(w, r, &body); err != nil {
			writeOrderProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		requestID := httpx.RequestID(r.Context())
		if requestID == "" {
			requestID = r.Header.Get("X-Request-ID")
		}
		order, err := service.Checkout(r.Context(), CheckoutCommand{
			CartID: body.CartID, AddressID: body.AddressID, IdempotencyKey: key,
			RequestID: requestID, Traceparent: r.Header.Get("traceparent"), CorrelationID: requestID,
		})
		if writeOrderServiceError(w, r, err) {
			return
		}
		status := http.StatusCreated
		if order.Replayed {
			status = http.StatusOK
		}
		writeOrderJSON(w, status, order)
	})
	mux.HandleFunc("GET /api/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		pageSize, err := parseOrderPageSize(r)
		if err != nil {
			writeOrderProblem(w, r, http.StatusBadRequest, "invalid_page_size")
			return
		}
		page, err := service.ListOrders(r.Context(), r.URL.Query().Get("cursor"), pageSize)
		if errors.Is(err, errInvalidOrderCursor) {
			writeOrderProblem(w, r, http.StatusBadRequest, "invalid_cursor")
			return
		}
		if writeOrderServiceError(w, r, err) {
			return
		}
		writeOrderJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		order, err := service.GetOrder(r.Context(), r.PathValue("id"))
		if writeOrderServiceError(w, r, err) {
			return
		}
		writeOrderJSON(w, http.StatusOK, order)
	})
	mux.HandleFunc("POST /api/v1/orders/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !requireOrderJSON(w, r) {
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeOrderProblem(w, r, http.StatusBadRequest, "idempotency_key_required")
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if err := decodeOrderJSON(w, r, &body); err != nil {
			writeOrderProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		requestID := httpx.RequestID(r.Context())
		if requestID == "" {
			requestID = r.Header.Get("X-Request-ID")
		}
		order, err := service.Cancel(r.Context(), CancelCommand{
			OrderID: r.PathValue("id"), Reason: body.Reason, CorrelationID: requestID,
		})
		if writeOrderServiceError(w, r, err) {
			return
		}
		writeOrderJSON(w, http.StatusOK, order)
	})
	return mux
}

func mutateCartHTTP(w http.ResponseWriter, r *http.Request, service *Service, action CartMutationAction, pathSKU string) {
	if !requireOrderJSON(w, r) {
		return
	}
	var body struct {
		SKU             string `json:"sku"`
		Quantity        int64  `json:"quantity"`
		ExpectedVersion int64  `json:"expected_version"`
	}
	if err := decodeOrderJSON(w, r, &body); err != nil {
		writeOrderProblem(w, r, http.StatusBadRequest, "invalid_json")
		return
	}
	if pathSKU != "" {
		body.SKU = pathSKU
	}
	cart, err := service.MutateCart(r.Context(), CartMutation{
		CartID: r.PathValue("id"), SKU: body.SKU, Quantity: body.Quantity,
		ExpectedVersion: body.ExpectedVersion, Action: action,
	})
	if writeOrderServiceError(w, r, err) {
		return
	}
	writeCart(w, http.StatusOK, cart)
}

func requireOrderJSON(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeOrderProblem(w, r, http.StatusUnsupportedMediaType, "json_content_type_required")
		return false
	}
	return true
}

func decodeOrderJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, orderBodyLimit))
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

func parseOrderPageSize(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("page_size")
	if raw == "" {
		return 0, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size <= 0 {
		return 0, errors.New("invalid page size")
	}
	return size, nil
}

func writeCart(w http.ResponseWriter, status int, cart Cart) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(cart.Version, 10)))
	writeOrderJSON(w, status, cart)
}

func writeOrderJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeOrderProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	httpx.WriteProblem(w, httpx.Problem{Status: status, Code: code, Instance: r.URL.Path})
}

func writeOrderServiceError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrCartNotFound):
		writeOrderProblem(w, r, http.StatusNotFound, "cart_not_found")
	case errors.Is(err, ErrOrderNotFound):
		writeOrderProblem(w, r, http.StatusNotFound, "order_not_found")
	case errors.Is(err, ErrVersionConflict):
		writeOrderProblem(w, r, http.StatusConflict, "cart_version_conflict")
	case errors.Is(err, ErrIdempotencyConflict):
		writeOrderProblem(w, r, http.StatusConflict, "idempotency_conflict")
	case errors.Is(err, ErrCheckoutUncertain), errors.Is(err, ErrUpstreamUnavailable):
		writeOrderProblem(w, r, http.StatusServiceUnavailable, "checkout_unavailable")
	case errors.Is(err, ErrInvalidCommand), errors.Is(err, ErrInvalidMoney):
		writeOrderProblem(w, r, http.StatusUnprocessableEntity, "invalid_order")
	default:
		writeOrderProblem(w, r, http.StatusInternalServerError, "internal_error")
	}
	return true
}
