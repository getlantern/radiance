package traces

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptrace"
	"strings"

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

type headerAnnotatingRoundTripper struct {
	next http.RoundTripper
}

func (h *headerAnnotatingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	span := trace.SpanFromContext(ctx)

	if span != nil {
		for k, v := range req.Header {
			span.SetAttributes(
				attribute.StringSlice(fmt.Sprintf("http.request.header.%s", strings.ToLower(k)), v),
			)
		}
	}

	return h.next.RoundTrip(req)
}

// NewHeaderAnnotatingRoundTripper reads the request headers during the roundtrip
// operation and adds the information as attributes. Please be aware that
// requests must have a context otherwise the info won't be added.
func NewHeaderAnnotatingRoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &headerAnnotatingRoundTripper{next: base}
}
