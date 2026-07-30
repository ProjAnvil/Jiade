package risk

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
	"bank/internal/risk/domain"
	"bank/internal/risk/service"
)

// stubService satisfies nothing — it's used through a wrapper. We test the
// consumer's pure functions (payload decode, result envelope building) and the
// dispatch routing without needing a real DB or AMQP.

func TestProcessEnvelope_UnknownMessageType(t *testing.T) {
	consumer := newTestConsumer(nil)
	env := messaging.Envelope{MessageType: "risk.unknown.v1"}
	err := consumer.processEnvelope(context.Background(), nil, env)
	if err == nil {
		t.Fatal("expected error for unknown message type")
	}
}

func TestProcessEnvelope_AuthorizePayment_DecodesPayload(t *testing.T) {
	// The dispatch should reach the authorize handler and fail on the store
	// lookup (nil DBTX → panic-free sql error from the service's store).
	// Instead of testing DB behavior, verify the payload decode + dispatch path
	// by using a service whose customer lookup returns an error immediately.
	svc := service.NewAuthorizationService(
		failingStore{},
		fakeCustomerReader{err: errors.New("test")},
		fixedNowConsumer,
	)
	consumer := newTestConsumer(svc)

	payload := authorizePaymentPayload{
		AuthorizationID: "auth-1",
		CustomerID:      "C1",
		AmountCents:     10000,
		Currency:        "CNY",
	}
	body, _ := json.Marshal(payload)
	env := messaging.Envelope{
		MessageType:    CmdAuthorizePayment,
		MessageID:      testUUID(),
		CorrelationID:  testUUID(),
		SchemaVersion:  messaging.CurrentSchemaVersion,
		OccurredAt:     time.Now(),
		WorkflowID:     "wf-1",
		IdempotencyKey: "idem-1",
		Payload:        body,
	}
	err := consumer.processEnvelope(context.Background(), nil, env)
	if err == nil {
		t.Fatal("expected error from customer lookup failure")
	}
}

func TestProcessEnvelope_VoidAuthorization_DecodesPayload(t *testing.T) {
	svc := service.NewAuthorizationService(
		failingStore{},
		fakeCustomerReader{},
		fixedNowConsumer,
	)
	consumer := newTestConsumer(svc)

	payload := voidAuthorizationPayload{
		AuthorizationID: "auth-1",
	}
	body, _ := json.Marshal(payload)
	env := messaging.Envelope{
		MessageType:   CmdVoidAuthorization,
		MessageID:     testUUID(),
		CorrelationID: testUUID(),
		SchemaVersion: messaging.CurrentSchemaVersion,
		OccurredAt:    time.Now(),
		Payload:       body,
	}
	// failingStore.GetByID returns ErrAuthorizationNotFound.
	err := consumer.processEnvelope(context.Background(), nil, env)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, service.ErrAuthorizationNotFound) {
		t.Errorf("expected ErrAuthorizationNotFound, got: %v", err)
	}
}

// --- result envelope builder tests ---

func TestBuildAuthorizeResultEnvelope_Authorized(t *testing.T) {
	cmdEnv := makeCommandEnvelope(CmdAuthorizePayment)
	auth := domain.PaymentAuthorization{
		AuthorizationID: "auth-1", WorkflowID: "wf-1", CustomerID: "C1",
		AmountCents: 10000, Currency: "CNY",
		MatchedRuleIDs: []string{},
		ContextDigest:  "digest-abc",
	}
	result := service.AuthorizeResult{
		Authorization: auth,
		EventType:     service.AuthorizeEventAuthorized,
	}
	env, route, err := buildAuthorizeResultEnvelope(cmdEnv, result, fixedNowConsumer())
	if err != nil {
		t.Fatal(err)
	}
	if route != RoutePaymentAuthorized {
		t.Errorf("route = %q, want %q", route, RoutePaymentAuthorized)
	}
	if env.MessageType != service.AuthorizeEventAuthorized {
		t.Errorf("message type = %q", env.MessageType)
	}
	if env.WorkflowID != "wf-1" {
		t.Errorf("workflow_id = %q", env.WorkflowID)
	}
	if env.CausationID != cmdEnv.MessageID {
		t.Errorf("causation_id = %q, want %q", env.CausationID, cmdEnv.MessageID)
	}
	if env.CorrelationID != cmdEnv.CorrelationID {
		t.Errorf("correlation_id mismatch")
	}
	var payload authorizeResultPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AuthorizationID != "auth-1" || payload.AmountCents != 10000 {
		t.Errorf("payload mismatch: %+v", payload)
	}
	if payload.ContextDigest != "digest-abc" {
		t.Errorf("digest = %q", payload.ContextDigest)
	}
}

func TestBuildAuthorizeResultEnvelope_Rejected(t *testing.T) {
	cmdEnv := makeCommandEnvelope(CmdAuthorizePayment)
	auth := domain.PaymentAuthorization{
		AuthorizationID: "auth-1", WorkflowID: "wf-1", CustomerID: "C1",
		MatchedRuleIDs: []string{domain.RuleKYCInactive},
		ContextDigest:  "digest-xyz",
	}
	result := service.AuthorizeResult{
		Authorization: auth,
		EventType:     service.AuthorizeEventRejected,
	}
	env, route, err := buildAuthorizeResultEnvelope(cmdEnv, result, fixedNowConsumer())
	if err != nil {
		t.Fatal(err)
	}
	if route != RoutePaymentRejected {
		t.Errorf("route = %q, want %q", route, RoutePaymentRejected)
	}
	if env.MessageType != service.AuthorizeEventRejected {
		t.Errorf("message type = %q", env.MessageType)
	}
	var payload authorizeResultPayload
	_ = json.Unmarshal(env.Payload, &payload)
	if len(payload.MatchedRules) != 1 || payload.MatchedRules[0] != domain.RuleKYCInactive {
		t.Errorf("matched rules = %v", payload.MatchedRules)
	}
}

func TestBuildVoidResultEnvelope(t *testing.T) {
	cmdEnv := makeCommandEnvelope(CmdVoidAuthorization)
	auth := domain.PaymentAuthorization{
		AuthorizationID: "auth-1", WorkflowID: "wf-1",
		Status: domain.AuthStatusVoided,
	}
	result := service.VoidResult{
		Authorization: auth,
		EventType:     service.VoidEventVoided,
	}
	env := buildVoidResultEnvelope(cmdEnv, result, fixedNowConsumer())
	if env.MessageType != service.VoidEventVoided {
		t.Errorf("message type = %q", env.MessageType)
	}
	if env.CausationID != cmdEnv.MessageID {
		t.Errorf("causation_id = %q, want %q", env.CausationID, cmdEnv.MessageID)
	}
	if env.WorkflowID != "wf-1" {
		t.Errorf("workflow_id = %q", env.WorkflowID)
	}
}

// TestPayloadIdempotencyKey verifies the fallback to the envelope's key.
func TestPayloadIdempotencyKey(t *testing.T) {
	env := messaging.Envelope{IdempotencyKey: "env-key"}
	if got := payloadIdempotencyKey(env, ""); got != "env-key" {
		t.Errorf("got %q, want env-key", got)
	}
	if got := payloadIdempotencyKey(env, "payload-key"); got != "payload-key" {
		t.Errorf("got %q, want payload-key", got)
	}
}

// --- helpers ---

func newTestConsumer(svc *service.AuthorizationService) *Consumer {
	if svc == nil {
		svc = service.NewAuthorizationService(nil, nil, fixedNowConsumer)
	}
	return &Consumer{
		service: svc,
		now:     fixedNowConsumer,
	}
}

func fixedNowConsumer() time.Time {
	return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
}

func makeCommandEnvelope(msgType string) messaging.Envelope {
	return messaging.Envelope{
		MessageID:      testUUID(),
		MessageType:    msgType,
		SchemaVersion:  messaging.CurrentSchemaVersion,
		WorkflowID:     "wf-1",
		ActionName:     "risk-authorization",
		IdempotencyKey: "idem-1",
		CorrelationID:  testUUID(),
		OccurredAt:     time.Now(),
		Payload:        json.RawMessage(`{}`),
	}
}

func testUUID() string {
	return "12345678-1234-1234-1234-123456789abc"
}

// failingStore returns ErrAuthorizationNotFound for all lookups and errors for
// writes. Used to exercise dispatch + decode paths without a DB.
type failingStore struct{}

func (failingStore) Insert(context.Context, pg.DBTX, domain.PaymentAuthorization) error {
	return errors.New("not available")
}
func (failingStore) GetByID(context.Context, pg.DBTX, string) (domain.PaymentAuthorization, error) {
	return domain.PaymentAuthorization{}, service.ErrAuthorizationNotFound
}
func (failingStore) GetByIdempotencyKey(context.Context, pg.DBTX, string) (domain.PaymentAuthorization, error) {
	return domain.PaymentAuthorization{}, service.ErrAuthorizationNotFound
}
func (failingStore) UpdateStatus(context.Context, pg.DBTX, domain.PaymentAuthorization) error {
	return errors.New("not available")
}
func (failingStore) IsBlacklisted(context.Context, pg.DBTX, string) (bool, error) {
	return false, nil
}

type fakeCustomerReader struct {
	customer serviceclient.Customer
	err      error
}

func (f fakeCustomerReader) GetCustomer(_ context.Context, _, _ string) (serviceclient.Customer, error) {
	return f.customer, f.err
}
