package ops

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func NewRoundTripper(original http.RoundTripper) http.RoundTripper {
	if original == nil {
		original = http.DefaultTransport
	}
	return otelhttp.NewTransport(original)
}
