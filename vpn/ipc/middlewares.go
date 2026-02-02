package ipc

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/traces"
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

func tracer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer(tracerName).Start(r.Context(), r.URL.Path)
		defer span.End()

		r = r.WithContext(ctx)
		var buf bytes.Buffer
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		ww.Tee(&buf)
		next.ServeHTTP(ww, r)
		if ww.Status() >= 400 {
			traces.RecordError(ctx, fmt.Errorf("status %d: %s", ww.Status(), buf.String()))
		}
	})
}

func authPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := usrFromContext(r.Context())
		if !peerCanAccess(peer) {
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func peerCanAccess(peer usr) bool {
	return peer.uid != "" && peer.isAdmin
}
