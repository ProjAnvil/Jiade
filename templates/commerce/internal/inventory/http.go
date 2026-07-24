package inventory

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

const inventoryBodyLimit = 1 << 20

func NewHandler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/inventory", func(w http.ResponseWriter, r *http.Request) {
		pageSize, err := parseInventoryPageSize(r)
		if err != nil {
			writeInventoryProblem(w, r, http.StatusBadRequest, "invalid_page_size")
			return
		}
		page, err := service.ListLevels(r.Context(), r.URL.Query().Get("cursor"), pageSize)
		if errors.Is(err, errInvalidCursor) {
			writeInventoryProblem(w, r, http.StatusBadRequest, "invalid_cursor")
			return
		}
		if err != nil {
			writeInventoryProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeInventoryJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v1/inventory/{sku}", func(w http.ResponseWriter, r *http.Request) {
		levels, err := service.GetLevelsBySKU(r.Context(), r.PathValue("sku"))
		if errors.Is(err, ErrSKUNotFound) {
			writeInventoryProblem(w, r, http.StatusNotFound, "sku_not_found")
			return
		}
		if err != nil {
			writeInventoryProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeInventoryJSON(w, http.StatusOK, map[string]any{"sku": r.PathValue("sku"), "levels": levels})
	})
	mux.HandleFunc("GET /api/v1/reservations/{order_id}", func(w http.ResponseWriter, r *http.Request) {
		allocations, err := service.ListReservationsByOrder(r.Context(), r.PathValue("order_id"))
		if err != nil {
			writeInventoryProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeInventoryJSON(w, http.StatusOK, ReservationResult{
			OrderID: r.PathValue("order_id"), Allocations: allocations,
		})
	})
	mux.HandleFunc("POST /internal/v1/reservations", func(w http.ResponseWriter, r *http.Request) {
		if !isInventoryJSON(r.Header.Get("Content-Type")) {
			writeInventoryProblem(w, r, http.StatusUnsupportedMediaType, "json_content_type_required")
			return
		}
		key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if key == "" {
			writeInventoryProblem(w, r, http.StatusBadRequest, "idempotency_key_required")
			return
		}
		var request struct {
			OrderID string        `json:"order_id"`
			Lines   []ReserveLine `json:"lines"`
		}
		if err := decodeInventoryJSON(w, r, &request); err != nil {
			writeInventoryProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		result, err := service.Reserve(r.Context(), ReserveCommand{
			OrderID: request.OrderID, IdempotencyKey: key, Lines: request.Lines,
			CorrelationID: httpx.RequestID(r.Context()),
		})
		if writeInventoryServiceError(w, r, err) {
			return
		}
		status := http.StatusCreated
		if result.Existing {
			status = http.StatusOK
		}
		writeInventoryJSON(w, status, result)
	})
	mux.HandleFunc("POST /internal/v1/reservations/{order_id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if !isInventoryJSON(r.Header.Get("Content-Type")) {
			writeInventoryProblem(w, r, http.StatusUnsupportedMediaType, "json_content_type_required")
			return
		}
		var body struct{}
		if err := decodeInventoryJSON(w, r, &body); err != nil {
			writeInventoryProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		var event ReservationEvent
		switch r.PathValue("action") {
		case "release":
			event = ReservationRelease
		case "commit":
			event = ReservationCommit
		case "expire":
			event = ReservationExpire
		default:
			writeInventoryProblem(w, r, http.StatusNotFound, "reservation_action_not_found")
			return
		}
		result, err := service.TransitionOrder(r.Context(), r.PathValue("order_id"), event)
		if writeInventoryServiceError(w, r, err) {
			return
		}
		writeInventoryJSON(w, http.StatusOK, result)
	})
	return mux
}

func writeInventoryServiceError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrInvalidCommand):
		writeInventoryProblem(w, r, http.StatusBadRequest, "invalid_reservation")
	case errors.Is(err, ErrSKUNotFound):
		writeInventoryProblem(w, r, http.StatusNotFound, "sku_not_found")
	case errors.Is(err, ErrInsufficientStock):
		writeInventoryProblem(w, r, http.StatusConflict, "insufficient_stock")
	case errors.Is(err, ErrIdempotencyConflict):
		writeInventoryProblem(w, r, http.StatusConflict, "idempotency_conflict")
	case errors.Is(err, ErrOrderTerminal):
		writeInventoryProblem(w, r, http.StatusConflict, "order_terminal")
	case errors.Is(err, ErrReservationNotFound):
		writeInventoryProblem(w, r, http.StatusNotFound, "reservation_not_found")
	default:
		writeInventoryProblem(w, r, http.StatusInternalServerError, "internal_error")
	}
	return true
}

func decodeInventoryJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, inventoryBodyLimit))
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

func isInventoryJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

func parseInventoryPageSize(r *http.Request) (int, error) {
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

func writeInventoryJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeInventoryProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	httpx.WriteProblem(w, httpx.Problem{Status: status, Code: code, Instance: r.URL.Path})
}
