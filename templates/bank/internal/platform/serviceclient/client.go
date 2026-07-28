// Package serviceclient provides read-only gRPC clients between bank services.
package serviceclient

import (
	"database/sql"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func mapNotFound(err error) error {
	if status.Code(err) == codes.NotFound {
		return fmt.Errorf("%w: %v", sql.ErrNoRows, err)
	}
	return err
}
