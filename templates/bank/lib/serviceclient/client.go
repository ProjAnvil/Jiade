// Package serviceclient provides read-only gRPC clients between bank services.
package serviceclient

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-chi/chi/v5/middleware"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RequestID extracts the optional inbound request ID for propagation to gRPC.
// It returns an empty string for contexts outside the REST request pipeline.
func RequestID(ctx context.Context) string { return middleware.GetReqID(ctx) }

func mapNotFound(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %v", sql.ErrNoRows, err)
	}
	return err
}
