// Package service is the use case layer of the risk service.
//
// This file adds the AuthorizationService that orchestrates payment
// authorization and void for the bank payment saga. It evaluates the
// deterministic risk policy, persists the authorization via AuthorizationStore,
// and classifies the outcome so the consumer knows which result event to emit.
package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"bank/internal/platform/pg"
	"bank/internal/platform/serviceclient"
	"bank/internal/risk/domain"
)

// ErrAuthorizationNotFound is returned when an authorization lookup misses.
// Repositories translate sql.ErrNoRows to this sentinel so callers can errors.Is.
var ErrAuthorizationNotFound = errors.New("authorization not found")

// Result event type constants. These are the message_type values the consumer
// stamps onto result envelopes emitted to the outbox.
const (
	AuthorizeEventAuthorized = "risk.payment-authorized.v1"
	AuthorizeEventRejected   = "risk.payment-rejected.v1"
	VoidEventVoided          = "risk.payment-authorization-voided.v1"
)

// AuthorizationStore is the persistence port for payment authorizations. All
// methods accept a pg.DBTX so the consumer can run Inbox insert, domain
// mutation, and Outbox write in a single transaction.
type AuthorizationStore interface {
	Insert(ctx context.Context, q pg.DBTX, auth domain.PaymentAuthorization) error
	GetByID(ctx context.Context, q pg.DBTX, authorizationID string) (domain.PaymentAuthorization, error)
	GetByIdempotencyKey(ctx context.Context, q pg.DBTX, key string) (domain.PaymentAuthorization, error)
	UpdateStatus(ctx context.Context, q pg.DBTX, auth domain.PaymentAuthorization) error
	IsBlacklisted(ctx context.Context, q pg.DBTX, customerID string) (bool, error)
}

// AuthorizationService orchestrates risk authorization for the payment saga.
type AuthorizationService struct {
	store    AuthorizationStore
	customer serviceclient.CustomerReader
	now      func() time.Time
}

// NewAuthorizationService constructs an AuthorizationService. now defaults to
// time.Now when nil.
func NewAuthorizationService(store AuthorizationStore, customer serviceclient.CustomerReader, now func() time.Time) *AuthorizationService {
	if now == nil {
		now = time.Now
	}
	return &AuthorizationService{store: store, customer: customer, now: now}
}

// AuthorizeCommand carries the input needed to evaluate and persist a payment
// authorization. WorkflowID and IdempotencyKey are propagated from the command
// envelope by the consumer.
type AuthorizeCommand struct {
	AuthorizationID string
	WorkflowID      string
	IdempotencyKey  string
	CustomerID      string
	AmountCents     int64
	Currency        string
}

// VoidCommand carries the input needed to void an existing authorization.
type VoidCommand struct {
	AuthorizationID string
	WorkflowID      string
	IdempotencyKey  string
}

// AuthorizeResult is the outcome of AuthorizePayment. When Duplicate is true
// the authorization already existed and the consumer must NOT emit a second
// result event. EventType tells the consumer which message_type to stamp.
type AuthorizeResult struct {
	Authorization domain.PaymentAuthorization
	EventType     string
	Duplicate     bool
}

// VoidResult is the outcome of VoidAuthorization.
type VoidResult struct {
	Authorization domain.PaymentAuthorization
	EventType     string
	Duplicate     bool
}

// AuthorizePayment evaluates the deterministic risk policy for the command and
// persists the authorization. If an authorization with the same idempotency key
// already exists it is returned with Duplicate=true (no re-evaluation, no
// second event). The pg.DBTX argument allows the caller to participate the
// write in a surrounding transaction.
func (s *AuthorizationService) AuthorizePayment(ctx context.Context, q pg.DBTX, cmd AuthorizeCommand) (AuthorizeResult, error) {
	// 1. Idempotency: if the authorization already exists, return it without
	//    re-evaluating the policy or emitting a new event.
	existing, err := s.store.GetByIdempotencyKey(ctx, q, cmd.IdempotencyKey)
	if err != nil && !errors.Is(err, ErrAuthorizationNotFound) {
		return AuthorizeResult{}, fmt.Errorf("lookup by idempotency key: %w", err)
	}
	if err == nil {
		return AuthorizeResult{
			Authorization: existing,
			EventType:     authorizeEventType(existing),
			Duplicate:     true,
		}, nil
	}

	// 2. Load the customer snapshot for policy evaluation.
	customer, err := s.customer.GetCustomer(ctx, cmd.CustomerID, serviceclient.RequestID(ctx))
	if err != nil {
		return AuthorizeResult{}, fmt.Errorf("lookup customer %s: %w", cmd.CustomerID, err)
	}

	// 3. Check the risk-DB blacklist for this customer.
	blacklisted, err := s.store.IsBlacklisted(ctx, q, cmd.CustomerID)
	if err != nil {
		return AuthorizeResult{}, fmt.Errorf("check blacklist for %s: %w", cmd.CustomerID, err)
	}

	// 4. Evaluate the deterministic policy.
	decision := domain.EvaluatePolicy(domain.PolicyContext{
		CustomerID:     cmd.CustomerID,
		AmountCents:    cmd.AmountCents,
		KYCStatus:      customer.KYCStatus,
		CustomerStatus: customer.Status,
		Blacklisted:    blacklisted,
		RiskTags:       customer.RiskTags,
	})

	// 5. Apply the decision to a fresh authorization and persist.
	now := s.now()
	auth := domain.NewPaymentAuthorization(
		cmd.AuthorizationID, cmd.WorkflowID, cmd.CustomerID,
		cmd.AmountCents, cmd.Currency, cmd.IdempotencyKey, now,
	)
	if err := auth.Authorize(decision); err != nil {
		return AuthorizeResult{}, fmt.Errorf("apply authorization decision: %w", err)
	}
	if err := s.store.Insert(ctx, q, auth); err != nil {
		return AuthorizeResult{}, fmt.Errorf("persist authorization: %w", err)
	}

	return AuthorizeResult{
		Authorization: auth,
		EventType:     authorizeEventType(auth),
		Duplicate:     false,
	}, nil
}

// VoidAuthorization loads an authorization by ID and transitions it to voided.
// If the authorization is already voided it is returned with Duplicate=true
// (no second update, no second event). Voiding a non-authorized authorization
// is an invalid transition and returns the wrapped domain error.
func (s *AuthorizationService) VoidAuthorization(ctx context.Context, q pg.DBTX, cmd VoidCommand) (VoidResult, error) {
	auth, err := s.store.GetByID(ctx, q, cmd.AuthorizationID)
	if err != nil {
		return VoidResult{}, err
	}
	if auth.Status == domain.AuthStatusVoided {
		return VoidResult{
			Authorization: auth,
			EventType:     VoidEventVoided,
			Duplicate:     true,
		}, nil
	}
	if err := auth.Void(); err != nil {
		return VoidResult{}, err
	}
	auth.UpdatedAt = s.now()
	if err := s.store.UpdateStatus(ctx, q, auth); err != nil {
		return VoidResult{}, fmt.Errorf("persist void: %w", err)
	}
	return VoidResult{
		Authorization: auth,
		EventType:     VoidEventVoided,
		Duplicate:     false,
	}, nil
}

func authorizeEventType(a domain.PaymentAuthorization) string {
	switch a.Status {
	case domain.AuthStatusAuthorized:
		return AuthorizeEventAuthorized
	case domain.AuthStatusRejected:
		return AuthorizeEventRejected
	default:
		return AuthorizeEventAuthorized // callers should check Duplicate first
	}
}
