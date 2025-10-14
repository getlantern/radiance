// Package traces provides utilities for working with OpenTelemetry traces.
package traces

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

func RecordError(ctx context.Context, err error, options ...trace.EventOption) error {
	if err == nil {
		return nil
	}
	slog.Error("Error occurred", "error", err)
	span := trace.SpanFromContext(ctx)
	span.RecordError(err, options...)
	return err
}
