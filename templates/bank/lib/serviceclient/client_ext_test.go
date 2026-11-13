package serviceclient

// Tests for RequestID propagation and mapNotFound's NotFound translation.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRequestID(t *testing.T) {
	// Outside the REST pipeline the request id is empty.
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("RequestID(background) = %q, want empty", got)
	}
	// Inside the pipeline the chi middleware id is propagated to gRPC.
	ctx := context.WithValue(context.Background(), middleware.RequestIDKey, "req-42")
	if got := RequestID(ctx); got != "req-42" {
		t.Errorf("RequestID(pipeline) = %q, want req-42", got)
	}
}

func TestMapNotFound(t *testing.T) {
	notFound := status.Error(codes.NotFound, "customer gone")
	err := mapNotFound(notFound)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("gRPC NotFound must wrap sql.ErrNoRows, got %v", err)
	}
	// The original status text is embedded (mapNotFound formats it with %v).
	if !strings.Contains(err.Error(), "customer gone") {
		t.Errorf("original gRPC status text must be preserved, got %v", err)
	}

	other := status.Error(codes.Unavailable, "later")
	if got := mapNotFound(other); !errors.Is(got, other) {
		t.Errorf("non-NotFound codes must pass through unchanged, got %v", got)
	}
	if got := mapNotFound(errors.New("plain")); got == nil || got.Error() != "plain" {
		t.Errorf("plain errors must pass through unchanged, got %v", got)
	}
}
