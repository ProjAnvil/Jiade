// Package admin implements the PROTECTED operator gRPC surface for recovering
// stuck payment compensations.
//
// The admin service exposes two RPCs:
//
//   - RetryCompensation    — re-dispatch the compensation command for an
//     instance stuck in compensation_failed.
//   - RecordReconciliation — resolve a stuck compensation action with an
//     immutable external reconciliation reference AFTER
//     validating the current core-banking state.
//
// Every call is gated by an x-bank-operator-token metadata credential compared
// in CONSTANT TIME, and persists an IMMUTABLE workflow_operator_audit row in
// the same transaction as the workflow state change (atomic). The service MUST
// NOT be exposed on the public gateway; access is restricted by NetworkPolicy
// in addition to the token check.
package admin

import (
	"context"
	"crypto/subtle"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Metadata keys. The token authorizes the operation; the operator-id records
// WHO performed it for the audit trail. Both travel in gRPC metadata.
const (
	operatorTokenMetadataKey = "x-bank-operator-token"
	operatorIDMetadataKey    = "x-bank-operator-id"
	defaultOperatorID        = "unknown-operator"
)

// TokenVerifier checks the operator credential carried in the gRPC metadata.
// The admin service calls Verify before every RPC.
type TokenVerifier interface {
	Verify(ctx context.Context) error
}

// constantTimeVerifier validates the operator token against an expected value
// using crypto/subtle.ConstantTimeCompare to avoid timing side channels that
// could leak the expected token byte-by-byte.
type constantTimeVerifier struct {
	expected []byte
}

// NewTokenVerifier returns a TokenVerifier that accepts only the given token.
// An empty token fails closed: Verify rejects every call so a misconfigured
// (empty) operator token can never authenticate an operation.
func NewTokenVerifier(token string) TokenVerifier {
	return constantTimeVerifier{expected: []byte(token)}
}

func (v constantTimeVerifier) Verify(ctx context.Context) error {
	// Fail closed on misconfiguration: an empty expected token never accepts,
	// even if the caller also sends an empty token.
	if len(v.expected) == 0 {
		return status.Error(codes.Unauthenticated, "operator token not configured")
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing operator token")
	}
	// metadata.Get returns the values for the lower-cased key; missing key
	// yields an empty slice. Only INCOMING metadata is honoured — a token
	// placed in outgoing metadata (a common client mistake) is rejected.
	values := md.Get(operatorTokenMetadataKey)
	if len(values) == 0 || values[0] == "" {
		return status.Error(codes.Unauthenticated, "missing operator token")
	}
	if subtle.ConstantTimeCompare([]byte(values[0]), v.expected) != 1 {
		return status.Error(codes.Unauthenticated, "invalid operator token")
	}
	return nil
}

// operatorIDFromMetadata extracts the claimed operator identity from the
// x-bank-operator-id metadata key for the audit record. The identity is
// attested (not authenticated) — the token authorizes the operation, the
// operator-id records who claims to have performed it. Missing → sentinel.
func operatorIDFromMetadata(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if values := md.Get(operatorIDMetadataKey); len(values) > 0 && values[0] != "" {
			return values[0]
		}
	}
	return defaultOperatorID
}
