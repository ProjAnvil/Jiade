package customer

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"commerce/internal/platform/httpx"
)

const customerBodyLimit = 1 << 20

func NewHandler(service *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/customers", func(w http.ResponseWriter, r *http.Request) {
		pageSize, err := parseCustomerPageSize(r)
		if err != nil {
			writeCustomerProblem(w, r, http.StatusBadRequest, "invalid_page_size")
			return
		}
		page, err := service.ListCustomers(r.Context(), r.URL.Query().Get("cursor"), pageSize)
		if errors.Is(err, errInvalidCursor) {
			writeCustomerProblem(w, r, http.StatusBadRequest, "invalid_cursor")
			return
		}
		if err != nil {
			writeCustomerProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeCustomerJSON(w, http.StatusOK, page)
	})
	mux.HandleFunc("GET /api/v1/customers/{id}", func(w http.ResponseWriter, r *http.Request) {
		customer, err := service.GetCustomer(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrCustomerNotFound) {
			writeCustomerProblem(w, r, http.StatusNotFound, "customer_not_found")
			return
		}
		if err != nil {
			writeCustomerProblem(w, r, http.StatusInternalServerError, "internal_error")
			return
		}
		writeCustomerJSON(w, http.StatusOK, customer)
	})
	mux.HandleFunc("POST /internal/v1/customer-addresses/validate", func(w http.ResponseWriter, r *http.Request) {
		if !isCustomerJSON(r.Header.Get("Content-Type")) {
			writeCustomerProblem(w, r, http.StatusUnsupportedMediaType, "json_content_type_required")
			return
		}
		var command struct {
			CustomerID string `json:"customer_id"`
			AddressID  string `json:"address_id"`
		}
		if err := decodeCustomerJSON(w, r, &command); err != nil {
			writeCustomerProblem(w, r, http.StatusBadRequest, "invalid_json")
			return
		}
		validation, err := service.ValidateAddress(r.Context(), command.CustomerID, command.AddressID)
		switch {
		case errors.Is(err, ErrAddressNotFound):
			writeCustomerProblem(w, r, http.StatusNotFound, "address_not_found")
		case errors.Is(err, ErrAddressNotUsable):
			writeCustomerProblem(w, r, http.StatusUnprocessableEntity, "address_not_usable")
		case err != nil:
			writeCustomerProblem(w, r, http.StatusInternalServerError, "internal_error")
		default:
			writeCustomerJSON(w, http.StatusOK, validation)
		}
	})
	return mux
}

func decodeCustomerJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, customerBodyLimit))
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

func isCustomerJSON(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

func parseCustomerPageSize(r *http.Request) (int, error) {
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

func writeCustomerJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeCustomerProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	httpx.WriteProblem(w, httpx.Problem{Status: status, Code: code, Instance: r.URL.Path})
}
