package ipc

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "github.com/getlantern/semconv"
	"go.opentelemetry.io/otel/trace"

	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/traces"
)

func logger(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pull the trace ID from the request, if it exists.
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		r = r.WithContext(ctx)
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(semconv.HTTPRouteKey.String(r.URL.Path))

		slog.Log(r.Context(), rlog.LevelTrace, "IPC request", "method", r.Method, "path", r.URL.Path)
		h.ServeHTTP(w, r)
	})
}

func tracer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer(tracerName).Start(r.Context(), r.URL.Path)
		defer span.End()

		r = r.WithContext(ctx)
		var buf bytes.Buffer
		ww := &statusRecorder{ResponseWriter: w, body: &buf}
		next.ServeHTTP(ww, r)
		if ww.status >= 400 {
			traces.RecordError(ctx, fmt.Errorf("status %d: %s", ww.status, buf.String()))
		}
	})
}

// statusRecorder wraps http.ResponseWriter to capture the status code and response body.
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   *bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// Flush implements http.Flusher if the underlying ResponseWriter supports it.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func authPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer := usrFromContext(r.Context())
		if peer.uid == "" {
			http.Error(w, "could not get credentials", http.StatusUnauthorized)
			return
		}
		if !peerCanAccess(peer) {
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func peerCanAccess(peer usr) bool {
	return peer.isAdmin
}
