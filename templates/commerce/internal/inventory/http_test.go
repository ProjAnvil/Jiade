package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"commerce/internal/platform/httpx"
)

func TestReserveReturnsExistingReservationForSameKey(t *testing.T) {
	store := newInventoryStoreStub()
	handler := NewHandler(NewService(store, fixedInventoryClock))
	body := `{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":2}]}`
	first := postInventoryJSON(handler, "/internal/v1/reservations", body, "reserve-1")
	second := postInventoryJSON(handler, "/internal/v1/reservations", body, "reserve-1")
	if first.Code != http.StatusCreated || second.Code != http.StatusOK ||
		first.Body.String() != second.Body.String() {
		t.Fatalf("first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
}

func TestReserveRejectsSameKeyWithDifferentPayload(t *testing.T) {
	store := newInventoryStoreStub()
	handler := NewHandler(NewService(store, fixedInventoryClock))
	postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":2}]}`, "reserve-1")
	response := postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":3}]}`, "reserve-1")
	assertInventoryProblem(t, response, http.StatusConflict, "idempotency_conflict")
}

func TestReserveMapsMissingSKUAndInsufficientStock(t *testing.T) {
	store := newInventoryStoreStub()
	handler := NewHandler(NewService(store, fixedInventoryClock))
	store.reserveErr = ErrSKUNotFound
	missing := postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-1","lines":[{"sku":"MISSING","quantity":1}]}`, "missing-key")
	assertInventoryProblem(t, missing, http.StatusNotFound, "sku_not_found")

	store.reserveErr = ErrInsufficientStock
	insufficient := postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":99}]}`, "insufficient-key")
	assertInventoryProblem(t, insufficient, http.StatusConflict, "insufficient_stock")
}

func TestReserveRequiresJSONIdempotencyKeyAndValidBody(t *testing.T) {
	handler := NewHandler(NewService(newInventoryStoreStub(), fixedInventoryClock))
	noKey := postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":1}]}`, "")
	assertInventoryProblem(t, noKey, http.StatusBadRequest, "idempotency_key_required")

	badJSON := postInventoryJSON(handler, "/internal/v1/reservations", `{"order_id":`, "key")
	assertInventoryProblem(t, badJSON, http.StatusBadRequest, "invalid_json")

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/reservations",
		strings.NewReader(`{"order_id":"ORD-1","lines":[]}`))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Idempotency-Key", "key")
	handler.ServeHTTP(response, request)
	assertInventoryProblem(t, response, http.StatusUnsupportedMediaType, "json_content_type_required")
}

func TestReservationTerminalOperationsAffectActiveRowsOnce(t *testing.T) {
	store := newInventoryStoreStub()
	handler := NewHandler(NewService(store, fixedInventoryClock))
	postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-1","lines":[{"sku":"SKU-1","quantity":2}]}`, "reserve-1")

	first := postInventoryJSON(handler, "/internal/v1/reservations/ORD-1/release", `{}`, "")
	second := postInventoryJSON(handler, "/internal/v1/reservations/ORD-1/release", `{}`, "")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || store.transitionChanges != 1 {
		t.Fatalf("first=%d second=%d transitionChanges=%d", first.Code, second.Code, store.transitionChanges)
	}
}

func TestTerminalBeforeReservePersistsFenceAndRejectsLateMutations(t *testing.T) {
	store := newInventoryStoreStub()
	handler := NewHandler(NewService(store, fixedInventoryClock))

	first := postInventoryJSON(handler, "/internal/v1/reservations/ORD-FENCED/release", `{}`, "")
	replay := postInventoryJSON(handler, "/internal/v1/reservations/ORD-FENCED/release", `{}`, "")
	different := postInventoryJSON(handler, "/internal/v1/reservations/ORD-FENCED/commit", `{}`, "")
	reserve := postInventoryJSON(handler, "/internal/v1/reservations",
		`{"order_id":"ORD-FENCED","lines":[{"sku":"SKU-1","quantity":1}]}`, "late-key")

	if first.Code != http.StatusOK || replay.Code != http.StatusOK {
		t.Fatalf("first=%d replay=%d", first.Code, replay.Code)
	}
	assertInventoryProblem(t, different, http.StatusConflict, "order_terminal")
	assertInventoryProblem(t, reserve, http.StatusConflict, "order_terminal")
}

func TestReserveReplayAndConflictTakePrecedenceAfterTerminal(t *testing.T) {
	tests := []struct {
		action string
		status ReservationState
	}{
		{action: "release", status: ReservationReleased},
		{action: "commit", status: ReservationCommitted},
		{action: "expire", status: ReservationExpired},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			store := newInventoryStoreStub()
			handler := NewHandler(NewService(store, fixedInventoryClock))
			body := `{"order_id":"ORD-REPLAY","lines":[{"sku":"SKU-1","quantity":1}]}`
			if response := postInventoryJSON(handler, "/internal/v1/reservations", body, "replay-key"); response.Code != http.StatusCreated {
				t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
			}
			if response := postInventoryJSON(handler, "/internal/v1/reservations/ORD-REPLAY/"+test.action, `{}`, ""); response.Code != http.StatusOK {
				t.Fatalf("terminal status=%d body=%s", response.Code, response.Body.String())
			}
			replay := postInventoryJSON(handler, "/internal/v1/reservations", body, "replay-key")
			if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"status":"`+string(test.status)+`"`) {
				t.Fatalf("replay status=%d body=%s", replay.Code, replay.Body.String())
			}
			conflict := postInventoryJSON(handler, "/internal/v1/reservations",
				`{"order_id":"ORD-REPLAY","lines":[{"sku":"SKU-1","quantity":2}]}`, "replay-key")
			assertInventoryProblem(t, conflict, http.StatusConflict, "idempotency_conflict")
		})
	}
}

func TestInventoryPublicReadsUseStablePagination(t *testing.T) {
	store := newInventoryStoreStub()
	store.levels = []InventoryLevel{
		{SKU: "SKU-1", LocationID: "LOC-1", LocationName: "One", Priority: 1, OnHand: 10, Reserved: 2, Available: 8},
		{SKU: "SKU-2", LocationID: "LOC-1", LocationName: "One", Priority: 1, OnHand: 5, Available: 5},
	}
	handler := NewHandler(NewService(store, fixedInventoryClock))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/inventory?page_size=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var page InventoryPage
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == "" || strings.Contains(page.NextCursor, "SKU-1") {
		t.Fatalf("page=%+v", page)
	}

	detail := httptest.NewRecorder()
	handler.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/v1/inventory/SKU-1", nil))
	if detail.Code != http.StatusOK || !strings.Contains(detail.Body.String(), `"available":8`) {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
}

func postInventoryJSON(handler http.Handler, path, body, key string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	handler.ServeHTTP(response, request)
	return response
}

func assertInventoryProblem(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("status=%d type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var problem httpx.Problem
	if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != code || problem.Instance == "" {
		t.Fatalf("problem=%+v", problem)
	}
}

func fixedInventoryClock() time.Time {
	return time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
}

type inventoryStoreStub struct {
	reservations      map[string]ReservationResult
	terminals         map[string]ReservationEvent
	levels            []InventoryLevel
	reserveErr        error
	transitionChanges int
}

func newInventoryStoreStub() *inventoryStoreStub {
	return &inventoryStoreStub{
		reservations: make(map[string]ReservationResult),
		terminals:    make(map[string]ReservationEvent),
	}
}

func (store *inventoryStoreStub) ListLevels(_ context.Context, after InventoryCursor, limit int) ([]InventoryLevel, error) {
	start := 0
	for start < len(store.levels) &&
		(store.levels[start].SKU < after.SKU ||
			(store.levels[start].SKU == after.SKU && store.levels[start].LocationID <= after.LocationID)) {
		start++
	}
	end := start + limit
	if end > len(store.levels) {
		end = len(store.levels)
	}
	return append([]InventoryLevel(nil), store.levels[start:end]...), nil
}

func (store *inventoryStoreStub) GetLevelsBySKU(_ context.Context, sku string) ([]InventoryLevel, error) {
	var found []InventoryLevel
	for _, level := range store.levels {
		if level.SKU == sku {
			found = append(found, level)
		}
	}
	if len(found) == 0 {
		return nil, ErrSKUNotFound
	}
	return found, nil
}

func (store *inventoryStoreStub) ListReservationsByOrder(_ context.Context, orderID string) ([]ReservationAllocation, error) {
	for _, result := range store.reservations {
		if result.OrderID == orderID {
			return append([]ReservationAllocation(nil), result.Allocations...), nil
		}
	}
	return nil, nil
}

func (store *inventoryStoreStub) Reserve(_ context.Context, command ReserveCommand) (ReservationResult, error) {
	if store.reserveErr != nil {
		return ReservationResult{}, store.reserveErr
	}
	if existing, ok := store.reservations[command.IdempotencyKey]; ok {
		if !sameReservePayload(existing, command) {
			return ReservationResult{}, ErrIdempotencyConflict
		}
		existing.Existing = true
		return existing, nil
	}
	if _, terminal := store.terminals[command.OrderID]; terminal {
		return ReservationResult{}, ErrOrderTerminal
	}
	result := ReservationResult{
		OrderID: command.OrderID,
		Allocations: []ReservationAllocation{{
			ID: "RES-1", OrderID: command.OrderID, SKU: command.Lines[0].SKU,
			LocationID: "LOC-1", Quantity: command.Lines[0].Quantity,
			State: ReservationActive, ExpiresAt: command.ExpiresAt,
		}},
	}
	store.reservations[command.IdempotencyKey] = result
	return result, nil
}

func (store *inventoryStoreStub) TransitionOrder(_ context.Context, orderID string, event ReservationEvent, _ time.Time) ([]ReservationAllocation, bool, error) {
	if terminal, ok := store.terminals[orderID]; ok {
		if terminal != event {
			return nil, false, ErrOrderTerminal
		}
		return store.allocationsForOrder(orderID), false, nil
	}
	store.terminals[orderID] = event
	for key, result := range store.reservations {
		if result.OrderID != orderID {
			continue
		}
		changed := false
		for index := range result.Allocations {
			if result.Allocations[index].State == ReservationActive {
				result.Allocations[index].State = reservationStateForEvent(event)
				changed = true
			}
		}
		if changed {
			store.transitionChanges++
			store.reservations[key] = result
		}
		return result.Allocations, changed, nil
	}
	store.transitionChanges++
	return nil, true, nil
}

func (store *inventoryStoreStub) allocationsForOrder(orderID string) []ReservationAllocation {
	for _, result := range store.reservations {
		if result.OrderID == orderID {
			return append([]ReservationAllocation(nil), result.Allocations...)
		}
	}
	return nil
}

func sameReservePayload(result ReservationResult, command ReserveCommand) bool {
	if result.OrderID != command.OrderID || len(result.Allocations) != len(command.Lines) {
		return false
	}
	return result.Allocations[0].SKU == command.Lines[0].SKU &&
		result.Allocations[0].Quantity == command.Lines[0].Quantity
}

func reservationStateForEvent(event ReservationEvent) ReservationState {
	switch event {
	case ReservationRelease:
		return ReservationReleased
	case ReservationCommit:
		return ReservationCommitted
	case ReservationExpire:
		return ReservationExpired
	default:
		panic(errors.New("unexpected reservation event"))
	}
}

var _ Store = (*inventoryStoreStub)(nil)
