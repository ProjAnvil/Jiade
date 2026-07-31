package corebanking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"bank/internal/corebanking/domain"
	"bank/internal/corebanking/service"
	"bank/internal/platform/messaging"
	"bank/internal/platform/pg"
	"bank/internal/platform/workflow"
)

// ---------------------------------------------------------------------------
// Stubs: HoldCommander, TransferCommander, OutboxWriter
// ---------------------------------------------------------------------------

// stubHoldCommander records every PlaceHold / ReleaseHold call.
type stubHoldCommander struct {
	placeHoldCalls   []domain.PlaceHoldInput
	placeHoldResult  domain.Hold
	placeHoldErr     error
	releaseHoldCalls []releaseHoldCall
	releaseHoldResult domain.Hold
	releaseHoldErr    error
}

type releaseHoldCall struct {
	HoldID       string
	IdempotencyKey string
}

func (s *stubHoldCommander) PlaceHold(_ context.Context, in domain.PlaceHoldInput) (domain.Hold, error) {
	s.placeHoldCalls = append(s.placeHoldCalls, in)
	if s.placeHoldErr != nil {
		return domain.Hold{}, s.placeHoldErr
	}
	return s.placeHoldResult, nil
}

func (s *stubHoldCommander) ReleaseHold(_ context.Context, holdID, idempotencyKey string) (domain.Hold, error) {
	s.releaseHoldCalls = append(s.releaseHoldCalls, releaseHoldCall{HoldID: holdID, IdempotencyKey: idempotencyKey})
	if s.releaseHoldErr != nil {
		return domain.Hold{}, s.releaseHoldErr
	}
	return s.releaseHoldResult, nil
}

// stubTransferCommander records every PostHeldTransfer / ReverseTransfer call.
type stubTransferCommander struct {
	postCalls     []service.PostHeldTransfer
	postResult    domain.Booking
	postErr       error
	reverseCalls  []service.ReverseTransfer
	reverseResult domain.Booking
	reverseErr    error
}

func (s *stubTransferCommander) PostHeldTransfer(_ context.Context, in service.PostHeldTransfer) (domain.Booking, error) {
	s.postCalls = append(s.postCalls, in)
	if s.postErr != nil {
		return domain.Booking{}, s.postErr
	}
	return s.postResult, nil
}

func (s *stubTransferCommander) ReverseTransfer(_ context.Context, in service.ReverseTransfer) (domain.Booking, error) {
	s.reverseCalls = append(s.reverseCalls, in)
	if s.reverseErr != nil {
		return domain.Booking{}, s.reverseErr
	}
	return s.reverseResult, nil
}

// capturingOutbox records every AppendOutbox call (envelope + routing key).
type capturingOutbox struct {
	msgs []capturedOutboxMsg
	err  error
}

type capturedOutboxMsg struct {
	Envelope   messaging.Envelope
	RoutingKey string
}

func (o *capturingOutbox) AppendOutbox(_ context.Context, _ pg.DBTX, env messaging.Envelope, routingKey string) error {
	if o.err != nil {
		return o.err
	}
	o.msgs = append(o.msgs, capturedOutboxMsg{Envelope: env, RoutingKey: routingKey})
	return nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func fixedNow() time.Time {
	return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
}

func testUUID() string {
	return "12345678-1234-1234-1234-123456789abc"
}

func newTestConsumer(t *testing.T) (*Consumer, *stubHoldCommander, *stubTransferCommander, *capturingOutbox) {
	t.Helper()
	holds := &stubHoldCommander{placeHoldResult: domain.Hold{HoldID: "H1", AccountNo: "D1", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY", Status: domain.HoldStatusActive}}
	transfers := &stubTransferCommander{postResult: domain.Booking{VoucherNo: "V1", BizDate: "2026-07-31"}, reverseResult: domain.Booking{VoucherNo: "V2", BizDate: "2026-07-31"}}
	outbox := &capturingOutbox{}
	c := NewConsumer(nil, holds, transfers, outbox, messaging.RetryPolicy{}, fixedNow)
	return c, holds, transfers, outbox
}

func makeEnvelope(msgType string, payload any) messaging.Envelope {
	body, _ := json.Marshal(payload)
	return messaging.Envelope{
		MessageID:      testUUID(),
		MessageType:    msgType,
		SchemaVersion:  messaging.CurrentSchemaVersion,
		WorkflowID:     "wf-1",
		ActionName:     "core-action",
		IdempotencyKey: "idem-1",
		CorrelationID:  testUUID(),
		OccurredAt:     fixedNow(),
		Payload:        body,
	}
}

// deliver simulates messaging.ProcessDelivery's Inbox dedup. The first delivery
// with a given MessageID invokes the handler; subsequent deliveries with the
// same MessageID are skipped (0 rows inserted by Inbox ON CONFLICT DO NOTHING).
// This mirrors the production guarantee without needing a real *sql.Tx.
func deliver(t *testing.T, handler func(context.Context, messaging.Envelope) error, seen map[string]bool, env messaging.Envelope) {
	t.Helper()
	if seen[env.MessageID] {
		return
	}
	seen[env.MessageID] = true
	if err := handler(context.Background(), env); err != nil {
		t.Fatalf("handler returned error on first delivery: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Step 1: Routing table tests — each command type maps to the right service
// ---------------------------------------------------------------------------

func TestConsumer_RoutingTable(t *testing.T) {
	tests := []struct {
		name          string
		messageType   string
		payload       any
		wantHoldCalls int
		wantPostCalls int
		wantRevCalls  int
		// wantOutboxType is the expected consumer-emitted outbox event type.
		// Empty means the consumer writes nothing (transfer success is emitted
		// by the service in its own transaction).
		wantOutboxType string
		wantRoute      string
	}{
		{
			name:          "place-hold routes to PlaceHold",
			messageType:   CmdPlaceHold,
			payload:       placeHoldPayload{AccountNo: "D1", AmountCents: 5000, Currency: "CNY"},
			wantHoldCalls: 1,
			wantOutboxType: EventHoldPlaced,
			wantRoute:      RouteHoldPlaced,
		},
		{
			name:          "release-hold routes to ReleaseHold",
			messageType:   CmdReleaseHold,
			payload:       releaseHoldPayload{HoldID: "H1"},
			wantHoldCalls: 1,
			wantOutboxType: EventHoldReleased,
			wantRoute:      RouteHoldReleased,
		},
		{
			name:          "post-held-transfer routes to PostHeldTransfer",
			messageType:   CmdPostHeldTransfer,
			payload:       postHeldTransferPayload{HoldID: "H1", FromAccount: "D1", ToAccount: "D2", AmountCents: 5000, Currency: "CNY"},
			wantPostCalls: 1,
			wantOutboxType: "", // service emits core.transfer-posted.v1
		},
		{
			name:         "reverse-transfer routes to ReverseTransfer",
			messageType:  CmdReverseTransfer,
			payload:      reverseTransferPayload{OriginalVoucherNo: "V1"},
			wantRevCalls: 1,
			wantOutboxType: "", // service emits core.transfer-reversed.v1
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, holds, transfers, outbox := newTestConsumer(t)
			env := makeEnvelope(tt.messageType, tt.payload)

			err := c.processEnvelope(context.Background(), nil, env)
			if err != nil {
				t.Fatalf("processEnvelope: %v", err)
			}

			if len(holds.placeHoldCalls)+len(holds.releaseHoldCalls) != tt.wantHoldCalls {
				t.Errorf("hold calls = %d+%d, want %d", len(holds.placeHoldCalls), len(holds.releaseHoldCalls), tt.wantHoldCalls)
			}
			if len(transfers.postCalls) != tt.wantPostCalls {
				t.Errorf("post calls = %d, want %d", len(transfers.postCalls), tt.wantPostCalls)
			}
			if len(transfers.reverseCalls) != tt.wantRevCalls {
				t.Errorf("reverse calls = %d, want %d", len(transfers.reverseCalls), tt.wantRevCalls)
			}

			if tt.wantOutboxType != "" {
				if len(outbox.msgs) != 1 {
					t.Fatalf("outbox msgs = %d, want 1", len(outbox.msgs))
				}
				if outbox.msgs[0].Envelope.MessageType != tt.wantOutboxType {
					t.Errorf("event type = %q, want %q", outbox.msgs[0].Envelope.MessageType, tt.wantOutboxType)
				}
				if outbox.msgs[0].RoutingKey != tt.wantRoute {
					t.Errorf("route = %q, want %q", outbox.msgs[0].RoutingKey, tt.wantRoute)
				}
			} else if len(outbox.msgs) != 0 {
				t.Errorf("outbox msgs = %d, want 0 (service emits its own event)", len(outbox.msgs))
			}
		})
	}
}

func TestConsumer_Routing_DecodesPayload(t *testing.T) {
	c, holds, transfers, _ := newTestConsumer(t)

	// place-hold
	env := makeEnvelope(CmdPlaceHold, placeHoldPayload{AccountNo: "D1", AmountCents: 5000, Currency: "CNY"})
	if err := c.processEnvelope(context.Background(), nil, env); err != nil {
		t.Fatalf("place-hold: %v", err)
	}
	if len(holds.placeHoldCalls) != 1 {
		t.Fatalf("place-hold calls = %d", len(holds.placeHoldCalls))
	}
	got := holds.placeHoldCalls[0]
	if got.AccountNo != "D1" || got.Amount.Cents() != 5000 || got.Ccy != "CNY" {
		t.Errorf("place-hold decoded: %+v", got)
	}
	if got.IdempotencyKey != "idem-1" {
		t.Errorf("idempotency key = %q, want idem-1 (from envelope)", got.IdempotencyKey)
	}
	if got.WorkflowID != "wf-1" {
		t.Errorf("workflow_id = %q, want wf-1", got.WorkflowID)
	}

	// release-hold
	env = makeEnvelope(CmdReleaseHold, releaseHoldPayload{HoldID: "H1"})
	if err := c.processEnvelope(context.Background(), nil, env); err != nil {
		t.Fatalf("release-hold: %v", err)
	}
	if len(holds.releaseHoldCalls) != 1 {
		t.Fatalf("release-hold calls = %d", len(holds.releaseHoldCalls))
	}
	rc := holds.releaseHoldCalls[0]
	if rc.HoldID != "H1" {
		t.Errorf("release-hold holdID = %q, want H1", rc.HoldID)
	}

	// post-held-transfer
	env = makeEnvelope(CmdPostHeldTransfer, postHeldTransferPayload{HoldID: "H1", FromAccount: "D1", ToAccount: "D2", AmountCents: 5000, Currency: "CNY", Summary: "payment"})
	if err := c.processEnvelope(context.Background(), nil, env); err != nil {
		t.Fatalf("post-held-transfer: %v", err)
	}
	if len(transfers.postCalls) != 1 {
		t.Fatalf("post calls = %d", len(transfers.postCalls))
	}
	pc := transfers.postCalls[0]
	if pc.HoldID != "H1" || pc.FromAccount != "D1" || pc.ToAccount != "D2" || pc.Amount.Cents() != 5000 || pc.Ccy != "CNY" {
		t.Errorf("post-held-transfer decoded: %+v", pc)
	}

	// reverse-transfer
	env = makeEnvelope(CmdReverseTransfer, reverseTransferPayload{OriginalVoucherNo: "V1", Summary: "reversal"})
	if err := c.processEnvelope(context.Background(), nil, env); err != nil {
		t.Fatalf("reverse-transfer: %v", err)
	}
	if len(transfers.reverseCalls) != 1 {
		t.Fatalf("reverse calls = %d", len(transfers.reverseCalls))
	}
	rv := transfers.reverseCalls[0]
	if rv.OriginalVoucherNo != "V1" {
		t.Errorf("reverse-transfer voucher = %q, want V1", rv.OriginalVoucherNo)
	}
}

// TestConsumer_Transfers_PropagateSagaRouting verifies the consumer threads
// the command envelope's saga-routing fields into the service input so the
// service-emitted transfer-posted/reversed result envelopes carry the full
// routing context the saga engine's ApplyResult correlates on (Bug 7).
func TestConsumer_Transfers_PropagateSagaRouting(t *testing.T) {
	c, _, transfers, _ := newTestConsumer(t)

	// post-held-transfer
	env := makeEnvelope(CmdPostHeldTransfer, postHeldTransferPayload{
		HoldID: "H1", FromAccount: "D1", ToAccount: "D2", AmountCents: 5000, Currency: "CNY",
	})
	env.CommandID = "cmd-post-1"
	if err := c.processEnvelope(context.Background(), nil, env); err != nil {
		t.Fatalf("post-held-transfer: %v", err)
	}
	if len(transfers.postCalls) != 1 {
		t.Fatalf("post calls = %d", len(transfers.postCalls))
	}
	pc := transfers.postCalls[0]
	if pc.SagaRouting.WorkflowID != env.WorkflowID {
		t.Errorf("post SagaRouting.WorkflowID = %q, want %q", pc.SagaRouting.WorkflowID, env.WorkflowID)
	}
	if pc.SagaRouting.ActionName != env.ActionName {
		t.Errorf("post SagaRouting.ActionName = %q, want %q", pc.SagaRouting.ActionName, env.ActionName)
	}
	if pc.SagaRouting.CommandID != "cmd-post-1" {
		t.Errorf("post SagaRouting.CommandID = %q, want cmd-post-1", pc.SagaRouting.CommandID)
	}
	if pc.SagaRouting.CorrelationID != env.CorrelationID {
		t.Errorf("post SagaRouting.CorrelationID = %q, want %q", pc.SagaRouting.CorrelationID, env.CorrelationID)
	}
	if pc.SagaRouting.CommandMessageID != env.MessageID {
		t.Errorf("post SagaRouting.CommandMessageID = %q, want %q", pc.SagaRouting.CommandMessageID, env.MessageID)
	}

	// reverse-transfer
	env = makeEnvelope(CmdReverseTransfer, reverseTransferPayload{OriginalVoucherNo: "V1"})
	env.CommandID = "cmd-rev-1"
	if err := c.processEnvelope(context.Background(), nil, env); err != nil {
		t.Fatalf("reverse-transfer: %v", err)
	}
	if len(transfers.reverseCalls) != 1 {
		t.Fatalf("reverse calls = %d", len(transfers.reverseCalls))
	}
	rc := transfers.reverseCalls[0]
	if rc.SagaRouting.WorkflowID != env.WorkflowID {
		t.Errorf("reverse SagaRouting.WorkflowID = %q, want %q", rc.SagaRouting.WorkflowID, env.WorkflowID)
	}
	if rc.SagaRouting.CommandID != "cmd-rev-1" {
		t.Errorf("reverse SagaRouting.CommandID = %q, want cmd-rev-1", rc.SagaRouting.CommandID)
	}
	if rc.SagaRouting.CorrelationID != env.CorrelationID {
		t.Errorf("reverse SagaRouting.CorrelationID = %q, want %q", rc.SagaRouting.CorrelationID, env.CorrelationID)
	}
	if rc.SagaRouting.CommandMessageID != env.MessageID {
		t.Errorf("reverse SagaRouting.CommandMessageID = %q, want %q", rc.SagaRouting.CommandMessageID, env.MessageID)
	}
}

// ---------------------------------------------------------------------------
// Step 1: Idempotency — deliver each message twice → one mutation + one
// outbox row (Inbox dedup simulated).
// ---------------------------------------------------------------------------

func TestConsumer_Idempotency_DoubleDelivery(t *testing.T) {
	tests := []struct {
		name        string
		messageType string
		payload     any
		// checkMutations asserts the domain service was called exactly once.
		checkMutations func(*testing.T, *stubHoldCommander, *stubTransferCommander)
		// wantOutbox is the number of consumer-emitted outbox messages.
		wantOutbox int
	}{
		{
			name:        "place-hold",
			messageType: CmdPlaceHold,
			payload:     placeHoldPayload{AccountNo: "D1", AmountCents: 5000, Currency: "CNY"},
			checkMutations: func(t *testing.T, h *stubHoldCommander, _ *stubTransferCommander) {
				if len(h.placeHoldCalls) != 1 {
					t.Errorf("PlaceHold calls = %d, want 1", len(h.placeHoldCalls))
				}
			},
			wantOutbox: 1, // core.hold-placed.v1
		},
		{
			name:        "release-hold",
			messageType: CmdReleaseHold,
			payload:     releaseHoldPayload{HoldID: "H1"},
			checkMutations: func(t *testing.T, h *stubHoldCommander, _ *stubTransferCommander) {
				if len(h.releaseHoldCalls) != 1 {
					t.Errorf("ReleaseHold calls = %d, want 1", len(h.releaseHoldCalls))
				}
			},
			wantOutbox: 1, // core.hold-released.v1
		},
		{
			name:        "post-held-transfer",
			messageType: CmdPostHeldTransfer,
			payload:     postHeldTransferPayload{HoldID: "H1", FromAccount: "D1", ToAccount: "D2", AmountCents: 5000, Currency: "CNY"},
			checkMutations: func(t *testing.T, _ *stubHoldCommander, tr *stubTransferCommander) {
				if len(tr.postCalls) != 1 {
					t.Errorf("PostHeldTransfer calls = %d, want 1", len(tr.postCalls))
				}
			},
			wantOutbox: 0, // service emits core.transfer-posted.v1 in its own tx
		},
		{
			name:        "reverse-transfer",
			messageType: CmdReverseTransfer,
			payload:     reverseTransferPayload{OriginalVoucherNo: "V1"},
			checkMutations: func(t *testing.T, _ *stubHoldCommander, tr *stubTransferCommander) {
				if len(tr.reverseCalls) != 1 {
					t.Errorf("ReverseTransfer calls = %d, want 1", len(tr.reverseCalls))
				}
			},
			wantOutbox: 0, // service emits core.transfer-reversed.v1 in its own tx
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, holds, transfers, outbox := newTestConsumer(t)
			env := makeEnvelope(tt.messageType, tt.payload)
			handler := func(ctx context.Context, env messaging.Envelope) error {
				return c.processEnvelope(ctx, nil, env)
			}

			seen := make(map[string]bool)
			deliver(t, handler, seen, env) // first delivery
			deliver(t, handler, seen, env) // second delivery (Inbox dedup skips handler)

			tt.checkMutations(t, holds, transfers)
			if len(outbox.msgs) != tt.wantOutbox {
				t.Errorf("outbox msgs = %d, want %d", len(outbox.msgs), tt.wantOutbox)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Step 3: ErrorClass mapping — domain errors map to the right outcome class
// ---------------------------------------------------------------------------

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want workflow.ErrorClass
	}{
		// insufficient funds → business_rejected
		{"insufficient available balance", service.ErrInsufficientAvailableBalance, workflow.BusinessRejected},
		{"insufficient balance", service.ErrInsufficientBalance, workflow.BusinessRejected},

		// domain invariant failures → invariant_violation
		{"hold captured", domain.ErrHoldCaptured, workflow.InvariantViolation},
		{"hold released", domain.ErrHoldReleased, workflow.InvariantViolation},
		{"hold invalid transition", domain.ErrInvalidHoldTransition, workflow.InvariantViolation},
		{"hold not active", service.ErrHoldNotActive, workflow.InvariantViolation},
		{"hold amount mismatch", service.ErrHoldAmountMismatch, workflow.InvariantViolation},
		{"hold ccy mismatch", service.ErrHoldCcyMismatch, workflow.InvariantViolation},
		{"hold account mismatch", service.ErrHoldAccountMismatch, workflow.InvariantViolation},
		{"hold not found", service.ErrHoldNotFound, workflow.InvariantViolation},
		{"non-positive hold amount", service.ErrNonPositiveHoldAmount, workflow.InvariantViolation},
		{"voucher already reversed", service.ErrVoucherAlreadyReversed, workflow.InvariantViolation},
		{"original voucher not found", service.ErrOriginalVoucherNotFound, workflow.InvariantViolation},
		{"account not found", service.ErrAccountNotFound, workflow.InvariantViolation},
		{"account not active", service.ErrAccountNotActive, workflow.InvariantViolation},
		{"ccy mismatch", service.ErrCcyMismatch, workflow.InvariantViolation},
		{"held transfer not found", service.ErrHeldTransferNotFound, workflow.InvariantViolation},

		// unknown / DB / broker → transient_failure
		{"generic error", errors.New("connection refused"), workflow.TransientFailure},
		{"nil", nil, workflow.TransientFailure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyError(tt.err)
			if got != tt.want {
				t.Errorf("classifyError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Step 3: Terminal failures emit failure events; transient errors return error
// ---------------------------------------------------------------------------

func TestConsumer_PlaceHold_InsufficientFunds_EmitsFailureEvent(t *testing.T) {
	c, holds, _, outbox := newTestConsumer(t)
	holds.placeHoldErr = service.ErrInsufficientAvailableBalance

	env := makeEnvelope(CmdPlaceHold, placeHoldPayload{AccountNo: "D1", AmountCents: 999999, Currency: "CNY"})
	err := c.processEnvelope(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("terminal failure should NOT return error (emit event + ack): %v", err)
	}
	if len(holds.placeHoldCalls) != 1 {
		t.Errorf("PlaceHold calls = %d, want 1", len(holds.placeHoldCalls))
	}
	if len(outbox.msgs) != 1 {
		t.Fatalf("outbox msgs = %d, want 1", len(outbox.msgs))
	}
	msg := outbox.msgs[0]
	if msg.Envelope.MessageType != EventHoldFailed {
		t.Errorf("event type = %q, want %q", msg.Envelope.MessageType, EventHoldFailed)
	}
	if msg.RoutingKey != RouteHoldFailed {
		t.Errorf("route = %q, want %q", msg.RoutingKey, RouteHoldFailed)
	}
	var payload failurePayload
	if err := json.Unmarshal(msg.Envelope.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ErrorClass != string(workflow.BusinessRejected) {
		t.Errorf("error_class = %q, want %q", payload.ErrorClass, workflow.BusinessRejected)
	}
}

func TestConsumer_PlaceHold_TransientFailure_ReturnsError(t *testing.T) {
	c, holds, _, outbox := newTestConsumer(t)
	dbErr := errors.New("connection refused")
	holds.placeHoldErr = dbErr

	env := makeEnvelope(CmdPlaceHold, placeHoldPayload{AccountNo: "D1", AmountCents: 5000, Currency: "CNY"})
	err := c.processEnvelope(context.Background(), nil, env)
	if err == nil {
		t.Fatal("transient failure should return error for retry")
	}
	if !errors.Is(err, dbErr) {
		t.Errorf("err = %v, want to wrap %v", err, dbErr)
	}
	if len(outbox.msgs) != 0 {
		t.Errorf("outbox msgs = %d, want 0 (no event for transient failure)", len(outbox.msgs))
	}
}

func TestConsumer_PostHeldTransfer_InvariantFailure_EmitsFailureEvent(t *testing.T) {
	c, _, transfers, outbox := newTestConsumer(t)
	transfers.postErr = fmt.Errorf("%w: hold H1 captured", service.ErrHoldNotActive)

	env := makeEnvelope(CmdPostHeldTransfer, postHeldTransferPayload{HoldID: "H1", FromAccount: "D1", ToAccount: "D2", AmountCents: 5000, Currency: "CNY"})
	err := c.processEnvelope(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("terminal failure should NOT return error: %v", err)
	}
	if len(outbox.msgs) != 1 {
		t.Fatalf("outbox msgs = %d, want 1", len(outbox.msgs))
	}
	if outbox.msgs[0].Envelope.MessageType != EventTransferFailed {
		t.Errorf("event type = %q, want %q", outbox.msgs[0].Envelope.MessageType, EventTransferFailed)
	}
	var payload failurePayload
	_ = json.Unmarshal(outbox.msgs[0].Envelope.Payload, &payload)
	if payload.ErrorClass != string(workflow.InvariantViolation) {
		t.Errorf("error_class = %q, want %q", payload.ErrorClass, workflow.InvariantViolation)
	}
}

func TestConsumer_UnknownMessageType_EmitsInvalidMessage(t *testing.T) {
	c, _, _, outbox := newTestConsumer(t)
	env := makeEnvelope("core.unknown-command.v1", map[string]any{})
	err := c.processEnvelope(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("unknown message type should NOT return error (emit invalid_message event + ack): %v", err)
	}
	if len(outbox.msgs) != 1 {
		t.Fatalf("outbox msgs = %d, want 1", len(outbox.msgs))
	}
	msg := outbox.msgs[0]
	if msg.Envelope.MessageType != EventCommandRejected {
		t.Errorf("event type = %q, want %q", msg.Envelope.MessageType, EventCommandRejected)
	}
	if msg.RoutingKey != RouteCommandRejected {
		t.Errorf("route = %q, want %q", msg.RoutingKey, RouteCommandRejected)
	}
	var payload failurePayload
	if err := json.Unmarshal(msg.Envelope.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ErrorClass != string(workflow.InvalidMessage) {
		t.Errorf("error_class = %q, want %q", payload.ErrorClass, workflow.InvalidMessage)
	}
}

func TestConsumer_DecodeFailure_EmitsInvalidMessage(t *testing.T) {
	c, _, _, outbox := newTestConsumer(t)
	// Malformed payload for a known command type.
	env := makeEnvelope(CmdPlaceHold, json.RawMessage(`{bad json`))
	err := c.processEnvelope(context.Background(), nil, env)
	if err != nil {
		t.Fatalf("decode failure should NOT return error (emit invalid_message event + ack): %v", err)
	}
	if len(outbox.msgs) != 1 {
		t.Fatalf("outbox msgs = %d, want 1", len(outbox.msgs))
	}
	var payload failurePayload
	_ = json.Unmarshal(outbox.msgs[0].Envelope.Payload, &payload)
	if payload.ErrorClass != string(workflow.InvalidMessage) {
		t.Errorf("error_class = %q, want %q", payload.ErrorClass, workflow.InvalidMessage)
	}
}

// ---------------------------------------------------------------------------
// Result envelope builder tests
// ---------------------------------------------------------------------------

func TestHoldPlacedResultEnvelope_PropagatesCorrelation(t *testing.T) {
	c, _, _, _ := newTestConsumer(t)
	cmdEnv := makeEnvelope(CmdPlaceHold, placeHoldPayload{AccountNo: "D1", AmountCents: 5000, Currency: "CNY"})
	hold := domain.Hold{HoldID: "H1", AccountNo: "D1", Amount: domain.NewMoneyFromCents(5000), Ccy: "CNY"}

	env, err := c.buildHoldPlacedEnvelope(context.Background(), nil, cmdEnv, hold)
	if err != nil {
		t.Fatal(err)
	}
	if env.MessageType != EventHoldPlaced {
		t.Errorf("type = %q", env.MessageType)
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
	if env.IdempotencyKey != cmdEnv.IdempotencyKey {
		t.Errorf("idempotency_key mismatch")
	}
	var payload holdPlacedPayload
	_ = json.Unmarshal(env.Payload, &payload)
	if payload.HoldID != "H1" || payload.AmountCents != 5000 {
		t.Errorf("payload mismatch: %+v", payload)
	}
}

func TestFailureResultEnvelope_PropagatesCorrelation(t *testing.T) {
	c, _, _, _ := newTestConsumer(t)
	cmdEnv := makeEnvelope(CmdReleaseHold, releaseHoldPayload{HoldID: "H1"})

	err := c.emitFailure(context.Background(), nil, cmdEnv, EventHoldReleaseFailed, RouteHoldReleaseFailed,
		workflow.InvariantViolation, "hold captured")
	if err != nil {
		t.Fatal(err)
	}
}
