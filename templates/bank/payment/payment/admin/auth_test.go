package admin

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// withToken returns a context carrying the operator-token metadata key in the
// INCOMING metadata (what a server reads). Uses MD.Append so it composes with
// withOperator.
func withToken(ctx context.Context, token string) context.Context {
	return appendIncoming(ctx, operatorTokenMetadataKey, token)
}

// withOperator returns a context carrying the operator-id metadata key.
func withOperator(ctx context.Context, operator string) context.Context {
	return appendIncoming(ctx, operatorIDMetadataKey, operator)
}

// appendIncoming adds a key/value to the incoming metadata on ctx, preserving
// any existing incoming metadata.
func appendIncoming(ctx context.Context, key, value string) context.Context {
	md, _ := metadata.FromIncomingContext(ctx)
	cp := md.Copy()
	cp.Append(key, value)
	return metadata.NewIncomingContext(ctx, cp)
}

func TestTokenVerifier_AcceptsMatchingToken(t *testing.T) {
	v := NewTokenVerifier("s3cret-operator-token")
	if err := v.Verify(withToken(context.Background(), "s3cret-operator-token")); err != nil {
		t.Fatalf("Verify returned %v, want nil", err)
	}
}

func TestTokenVerifier_RejectsMissingToken(t *testing.T) {
	v := NewTokenVerifier("s3cret-operator-token")
	err := v.Verify(context.Background())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestTokenVerifier_RejectsEmptyToken(t *testing.T) {
	v := NewTokenVerifier("s3cret-operator-token")
	err := v.Verify(withToken(context.Background(), ""))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestTokenVerifier_RejectsWrongToken(t *testing.T) {
	v := NewTokenVerifier("s3cret-operator-token")
	err := v.Verify(withToken(context.Background(), "wrong-token"))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestTokenVerifier_RejectsWhenNoTokenConfigured(t *testing.T) {
	// Fail closed: an empty expected token (misconfiguration) must NEVER accept.
	v := NewTokenVerifier("")
	err := v.Verify(withToken(context.Background(), ""))
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), codes.Unauthenticated, err)
	}
}

func TestOperatorIDFromMetadata(t *testing.T) {
	if got := operatorIDFromMetadata(context.Background()); got != defaultOperatorID {
		t.Fatalf("operatorID = %q, want %q", got, defaultOperatorID)
	}
	if got := operatorIDFromMetadata(withOperator(context.Background(), "ops-alice")); got != "ops-alice" {
		t.Fatalf("operatorID = %q, want %q", got, "ops-alice")
	}
}
