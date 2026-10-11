package kindling

import (
	"context"

	"github.com/getlantern/kindling"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// maxAttemptErr bounds the error text put on a span.
const maxAttemptErr = 256

// recordRace puts kindling's race on the request's span: the winner and each
// raced transport's outcome as attributes, for grouping in queries, plus one
// event per attempt with its error.
func recordRace(ctx context.Context, race kindling.Race) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		return
	}
	span.SetAttributes(attribute.String("kindling.winner", race.Winner))
	for _, a := range race.Attempts {
		prefix := "kindling." + a.Transport + "."
		attrs := []attribute.KeyValue{
			attribute.String(prefix+"outcome", string(a.Outcome)),
			attribute.Int64(prefix+"elapsed_ms", a.Elapsed.Milliseconds()),
		}
		if a.StatusCode != 0 {
			attrs = append(attrs, attribute.Int(prefix+"status", a.StatusCode))
		}
		span.SetAttributes(attrs...)

		event := []attribute.KeyValue{
			attribute.String("kindling.transport", a.Transport),
			attribute.String("kindling.outcome", string(a.Outcome)),
			attribute.Int("kindling.status", a.StatusCode),
			attribute.Int64("kindling.elapsed_ms", a.Elapsed.Milliseconds()),
		}
		if a.Err != nil {
			msg := a.Err.Error()
			if len(msg) > maxAttemptErr {
				msg = msg[:maxAttemptErr]
			}
			event = append(event, attribute.String("kindling.error", msg))
		}
		span.AddEvent("kindling.attempt", trace.WithAttributes(event...))
	}
}
