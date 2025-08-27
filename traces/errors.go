// Package traces provides utilities for working with OpenTelemetry traces.
package traces

import "go.opentelemetry.io/otel/trace"

func RecordError(span trace.Span, err error, options ...trace.EventOption) error {
	if err == nil {
		return nil
	}
	span.RecordError(err, options...)
	return err
}
