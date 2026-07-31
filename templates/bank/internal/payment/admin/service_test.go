package admin

import (
	"context"
	"fmt"
	"testing"
	"time"

	paymentv1 "bank/gen/bank/payment/v1"
	"bank/internal/platform/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeGateway is a CompensationGateway stub recording the audit facts it
// received and returning a configured transition / error.
type fakeGateway struct {
	retryErr          error
	resolveErr        error
	retryAudit        OperatorAudit
	resolveAudit      OperatorAudit
	retryTransition   Transition
	resolveTransition Transition
}

func (g *fakeGateway) RetryCompensation(_ context.Context, _ string, audit OperatorAudit) (Transition, error) {
	// Mirror the real PgGateway, which stamps the transition onto the audit.
	g.retryAudit = audit.withTransition(
		workflow.InstanceStatus(g.retryTransition.Prev),
		workflow.InstanceStatus(g.retryTransition.New))
	if g.retryErr != nil {
		return Transition{}, g.retryErr
	}
	return g.retryTransition, nil
}

func (g *fakeGateway) ResolveCompensation(_ context.Context, _, _ string, audit OperatorAudit) (Transition, error) {
	g.resolveAudit = audit.withTransition(
		workflow.InstanceStatus(g.resolveTransition.Prev),
		workflow.InstanceStatus(g.resolveTransition.New))
	if g.resolveErr != nil {
		return Transition{}, g.resolveErr
	}
	return g.resolveTransition, nil
}

// fakeReconciler is a Reconciler stub.
type fakeReconciler struct {
	err error
}

func (r fakeReconciler) ValidateReconciliation(context.Context, string, string) error {
	return r.err
}

func newTestServer(t *testing.T) (*Server, *fakeGateway, *fakeReconciler) {
	t.Helper()
	gw := &fakeGateway{
		retryTransition:   Transition{WorkflowID: "wf-1", Prev: "compensation_failed", New: "compensating"},
		resolveTransition: Transition{WorkflowID: "wf-1", Prev: "compensation_failed", New: "compensated"},
	}
	rec := &fakeReconciler{}
	srv := NewServer(Config{
		TokenVerifier: NewTokenVerifier("tok"),
		Gateway:       gw,
		Reconciler:    rec,
		Now:           func() time.Time { return time.Unix(1700000000, 0) },
	})
	return srv, gw, rec
}

// ---------------------------------------------------------------------------
// RecordReconciliation
// ---------------------------------------------------------------------------

func TestRecordReconciliation_RejectsMissingToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, err := srv.RecordReconciliation(context.Background(), &paymentv1.RecordReconciliationRequest{
		WorkflowId: "wf-1", ActionName: "PlaceFundsHold",
		ExternalReference: "REF-1", Reason: "manual",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestRecordReconciliation_RejectsWrongToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := withToken(context.Background(), "wrong")
	_, err := srv.RecordReconciliation(ctx, &paymentv1.RecordReconciliationRequest{
		WorkflowId: "wf-1", ActionName: "PlaceFundsHold",
		ExternalReference: "REF-1", Reason: "manual",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestRecordReconciliation_RejectsEmptyExternalReference(t *testing.T) {
	srv, gw, _ := newTestServer(t)
	ctx := withToken(withOperator(context.Background(), "ops-alice"), "tok")
	_, err := srv.RecordReconciliation(ctx, &paymentv1.RecordReconciliationRequest{
		WorkflowId: "wf-1", ActionName: "PlaceFundsHold",
		ExternalReference: "", Reason: "manual",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.InvalidArgument, err)
	}
	if gw.resolveAudit.Reference != "" {
		t.Fatalf("gateway must not be invoked when external_reference is empty")
	}
}

func TestRecordReconciliation_RejectsWhenReconciliationValidationFails(t *testing.T) {
	srv, gw, rec := newTestServer(t)
	rec.err = status.Error(codes.FailedPrecondition, "hold still active")
	ctx := withToken(withOperator(context.Background(), "ops-alice"), "tok")
	_, err := srv.RecordReconciliation(ctx, &paymentv1.RecordReconciliationRequest{
		WorkflowId: "wf-1", ActionName: "PlaceFundsHold",
		ExternalReference: "REF-1", Reason: "manual",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
	}
	if gw.resolveAudit.Reference != "" {
		t.Fatalf("gateway must not be invoked when reconciliation validation fails")
	}
}

func TestRecordReconciliation_PersistsAuditAndResolves(t *testing.T) {
	srv, gw, _ := newTestServer(t)
	ctx := withToken(withOperator(context.Background(), "ops-alice"), "tok")
	resp, err := srv.RecordReconciliation(ctx, &paymentv1.RecordReconciliationRequest{
		WorkflowId: "wf-1", ActionName: "PlaceFundsHold",
		ExternalReference: "REF-42", Reason: "manual release verified",
	})
	if err != nil {
		t.Fatalf("RecordReconciliation returned %v", err)
	}
	if resp.GetStatus() != "compensated" {
		t.Fatalf("status = %q, want %q", resp.GetStatus(), "compensated")
	}
	// Audit must capture operator identity, reference, reason, transition.
	if gw.resolveAudit.Operator != "ops-alice" {
		t.Errorf("audit Operator = %q, want %q", gw.resolveAudit.Operator, "ops-alice")
	}
	if gw.resolveAudit.Reference != "REF-42" {
		t.Errorf("audit Reference = %q, want %q", gw.resolveAudit.Reference, "REF-42")
	}
	if gw.resolveAudit.Reason != "manual release verified" {
		t.Errorf("audit Reason = %q, want %q", gw.resolveAudit.Reason, "manual release verified")
	}
	if gw.resolveAudit.PrevState != "compensation_failed" || gw.resolveAudit.NewState != "compensated" {
		t.Errorf("audit transition = %q→%q, want compensation_failed→compensated",
			gw.resolveAudit.PrevState, gw.resolveAudit.NewState)
	}
	if gw.resolveAudit.CreatedAt.IsZero() {
		t.Errorf("audit CreatedAt is zero")
	}
}

func TestRecordReconciliation_RejectsEmptyWorkflowID(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := withToken(context.Background(), "tok")
	_, err := srv.RecordReconciliation(ctx, &paymentv1.RecordReconciliationRequest{
		WorkflowId: "", ActionName: "PlaceFundsHold",
		ExternalReference: "REF-1",
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.InvalidArgument, err)
	}
}

// ---------------------------------------------------------------------------
// RetryCompensation
// ---------------------------------------------------------------------------

func TestRetryCompensation_RejectsMissingToken(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, err := srv.RetryCompensation(context.Background(), &paymentv1.RetryCompensationRequest{
		WorkflowId: "wf-1", Reason: "stuck",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestRetryCompensation_PersistsAuditAndRetries(t *testing.T) {
	srv, gw, _ := newTestServer(t)
	ctx := withToken(withOperator(context.Background(), "ops-bob"), "tok")
	resp, err := srv.RetryCompensation(ctx, &paymentv1.RetryCompensationRequest{
		WorkflowId: "wf-1", Reason: "transient broker outage",
	})
	if err != nil {
		t.Fatalf("RetryCompensation returned %v", err)
	}
	if resp.GetStatus() != "compensating" {
		t.Fatalf("status = %q, want %q", resp.GetStatus(), "compensating")
	}
	if gw.retryAudit.Operator != "ops-bob" {
		t.Errorf("audit Operator = %q, want %q", gw.retryAudit.Operator, "ops-bob")
	}
	if gw.retryAudit.Reason != "transient broker outage" {
		t.Errorf("audit Reason = %q, want %q", gw.retryAudit.Reason, "transient broker outage")
	}
}

func TestRetryCompensation_MapsNotFound(t *testing.T) {
	srv, gw, _ := newTestServer(t)
	gw.retryErr = workflow.ErrInstanceNotFound
	ctx := withToken(context.Background(), "tok")
	_, err := srv.RetryCompensation(ctx, &paymentv1.RetryCompensationRequest{
		WorkflowId: "wf-missing",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.NotFound, err)
	}
}

func TestRetryCompensation_MapsInvalidState(t *testing.T) {
	srv, gw, _ := newTestServer(t)
	gw.retryErr = fmt.Errorf("instance not resolvable: %w", workflow.ErrInvalidCompensationState)
	ctx := withToken(context.Background(), "tok")
	_, err := srv.RetryCompensation(ctx, &paymentv1.RetryCompensationRequest{
		WorkflowId: "wf-1",
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %s, want %s (err=%v)", status.Code(err), codes.FailedPrecondition, err)
	}
}

// Ensure incoming metadata (not outgoing) is the source of the token: a token
// placed in OUTGOING metadata (a common client mistake) must NOT authenticate.
func TestTokenVerifier_IgnoresOutgoingMetadata(t *testing.T) {
	v := NewTokenVerifier("tok")
	ctx := metadata.AppendToOutgoingContext(context.Background(), operatorTokenMetadataKey, "tok")
	if err := v.Verify(ctx); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("outgoing-metadata token should be rejected; got %v", err)
	}
}
