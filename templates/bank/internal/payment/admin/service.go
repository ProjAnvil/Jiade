package admin

import (
	"context"
	"errors"
	"time"

	paymentv1 "bank/gen/bank/payment/v1"
	"bank/internal/platform/workflow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// OperatorAudit is the immutable record persisted for every operator action.
// Every field is captured BEFORE the state transition completes (within the
// same transaction) so the audit is an exact, replayable account of who did
// what, why, and what changed.
type OperatorAudit struct {
	Operator  string
	Action    string
	Reference string
	Reason    string
	PrevState string
	NewState  string
	CreatedAt time.Time
}

// Transition is the result of an operator compensation operation: the workflow
// id and the previous → new instance status.
type Transition struct {
	WorkflowID string
	Prev       string
	New        string
}

// CompensationGateway performs the operator-driven state transition AND records
// the immutable audit row in ONE atomic transaction. The implementation is
// responsible for atomicity (the audit row commits with the state change or
// both roll back); the service layer only handles auth, request validation,
// and orchestration.
type CompensationGateway interface {
	RetryCompensation(ctx context.Context, workflowID string, audit OperatorAudit) (Transition, error)
	ResolveCompensation(ctx context.Context, workflowID, actionName string, audit OperatorAudit) (Transition, error)
}

// Config wires the admin service's dependencies.
type Config struct {
	TokenVerifier TokenVerifier
	Gateway       CompensationGateway
	Reconciler    Reconciler
	// Now supplies the audit timestamp. Defaults to time.Now when nil.
	Now func() time.Time
}

// Server implements paymentv1.WorkflowAdminServiceServer. It is the PROTECTED
// operator surface: every RPC verifies the operator token before delegating to
// the gateway, and every operation persists an immutable audit row.
type Server struct {
	paymentv1.UnimplementedWorkflowAdminServiceServer
	tokens    TokenVerifier
	gateway   CompensationGateway
	reconcile Reconciler
	now       func() time.Time
}

// Compile-time assertion that *Server satisfies the generated server interface.
var _ paymentv1.WorkflowAdminServiceServer = (*Server)(nil)

// NewServer constructs the admin gRPC server. cfg.Now defaults to time.Now.
func NewServer(cfg Config) *Server {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Server{
		tokens:    cfg.TokenVerifier,
		gateway:   cfg.Gateway,
		reconcile: cfg.Reconciler,
		now:       now,
	}
}

// RetryCompensation re-dispatches the compensation command for an instance
// stuck in compensation_failed. Auth-gated; records an immutable audit row.
func (s *Server) RetryCompensation(ctx context.Context, req *paymentv1.RetryCompensationRequest) (*paymentv1.WorkflowStatus, error) {
	if err := s.tokens.Verify(ctx); err != nil {
		return nil, err
	}
	if req.GetWorkflowId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_id is required")
	}
	audit := OperatorAudit{
		Operator:  operatorIDFromMetadata(ctx),
		Action:    "(retry-compensation)",
		Reference: "(operator-retry)",
		Reason:    req.GetReason(),
		CreatedAt: s.now(),
	}
	t, err := s.gateway.RetryCompensation(ctx, req.GetWorkflowId(), audit)
	if err != nil {
		return nil, mapGatewayError(err)
	}
	return &paymentv1.WorkflowStatus{
		WorkflowId:    t.WorkflowID,
		Status:        t.New,
		CurrentAction: "(retry)",
	}, nil
}

// RecordReconciliation resolves a stuck compensation action with an immutable
// external reconciliation reference. Auth-gated; requires a non-empty external
// reference; validates the current core-banking state via the Reconciler
// BEFORE the gateway mutates the workflow; records an immutable audit row.
func (s *Server) RecordReconciliation(ctx context.Context, req *paymentv1.RecordReconciliationRequest) (*paymentv1.WorkflowStatus, error) {
	if err := s.tokens.Verify(ctx); err != nil {
		return nil, err
	}
	if req.GetWorkflowId() == "" || req.GetActionName() == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_id and action_name are required")
	}
	// A compensation can be resolved ONLY with a non-empty external reference:
	// the reference is the immutable proof underpinning the resolution.
	if req.GetExternalReference() == "" {
		return nil, status.Error(codes.InvalidArgument, "external_reference is required")
	}
	// Validate the CURRENT external state before any mutation. The Reconciler
	// returns FailedPrecondition when the hold is not released, the reversal
	// voucher is missing, or balances do not reconcile.
	if err := s.reconcile.ValidateReconciliation(ctx, req.GetWorkflowId(), req.GetActionName()); err != nil {
		return nil, err
	}
	audit := OperatorAudit{
		Operator:  operatorIDFromMetadata(ctx),
		Action:    req.GetActionName(),
		Reference: req.GetExternalReference(),
		Reason:    req.GetReason(),
		CreatedAt: s.now(),
	}
	t, err := s.gateway.ResolveCompensation(ctx, req.GetWorkflowId(), req.GetActionName(), audit)
	if err != nil {
		return nil, mapGatewayError(err)
	}
	return &paymentv1.WorkflowStatus{
		WorkflowId:    t.WorkflowID,
		Status:        t.New,
		CurrentAction: req.GetActionName(),
	}, nil
}

// mapGatewayError translates engine/store errors into gRPC status codes. A
// not-found instance is NotFound; a state-precondition violation (wrong
// status/action) is FailedPrecondition; anything else is Internal.
func mapGatewayError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, workflow.ErrInstanceNotFound) {
		return status.Errorf(codes.NotFound, "workflow not found: %v", err)
	}
	if errors.Is(err, workflow.ErrInvalidCompensationState) || errors.Is(err, workflow.ErrInvalidMessage) {
		return status.Errorf(codes.FailedPrecondition, "workflow not in a resolvable state: %v", err)
	}
	return status.Errorf(codes.Internal, "compensation operation failed: %v", err)
}
