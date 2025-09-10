package traces

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// NewRoundTripper wraps the provided http.RoundTripper with OpenTelemetry instrumentation.
func NewRoundTripper(original http.RoundTripper) http.RoundTripper {
	if original == nil {
		original = http.DefaultTransport
	}
	return otelhttp.NewTransport(original, otelhttp.WithClientTrace(httpTrace))
}

func httpTrace(ctx context.Context) *httptrace.ClientTrace {
	span := trace.SpanFromContext(ctx)
	return &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			span.SetAttributes(attribute.String("host_port", hostPort))
		},
		TLSHandshakeStart: func() {
			span.SetAttributes(attribute.Bool("handshake_started", true))
		},
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			if err != nil {
				RecordError(ctx, err)
				return
			}
			span.SetAttributes(attribute.Bool("handshake_complete", cs.HandshakeComplete))
		},
		DNSStart: func(di httptrace.DNSStartInfo) {
			span.SetAttributes(attribute.String("dns_host", di.Host))
		},
		DNSDone: func(di httptrace.DNSDoneInfo) {
			RecordError(ctx, di.Err)
		},
		ConnectStart: func(network, addr string) {
			span.SetAttributes(attribute.String("network", network))
		},
		ConnectDone: func(network, addr string, err error) {
			RecordError(ctx, err)
		},
	}
}
