// Package telemetry contains lightweight observability helpers.
package telemetry

import (
	"io"
	"log/slog"
)

// NewJSONLogger builds a JSON structured logger at the supplied output.
func NewJSONLogger(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, nil))
}
