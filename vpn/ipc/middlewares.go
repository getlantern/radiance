package ipc

import (
	"log/slog"
	"net/http"

	"github.com/getlantern/radiance/internal"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"
)

func log(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pull the trace ID from the request, if it exists.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		r = r.WithContext(ctx)
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(semconv.HTTPRouteKey.String(r.URL.Path))

		slog.Log(r.Context(), internal.LevelTrace, "IPC request", "method", r.Method, "path", r.URL.Path)
		h.ServeHTTP(w, r)
	})
}
