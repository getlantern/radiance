package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/traces"
)

const tracerName = "github.com/getlantern/radiance/vpn/ipc"

// empty is a placeholder type for requests that do not expect a response body.
type empty struct{}

// sendRequest sends an HTTP request to the specified endpoint with the given method and data.
func sendRequest[T any](method, endpoint string, data any) (T, error) {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "sendRequest", trace.WithAttributes(attribute.String("endpoint", endpoint)))
	defer span.End()

	buf, err := json.Marshal(data)
	var res T
	if err != nil {
		return res, traces.RecordError(span, fmt.Errorf("failed to marshal payload: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL+endpoint, bytes.NewReader(buf))
	if err != nil {
		return res, err
	}
	client := &http.Client{
		Transport: traces.NewRoundTripper(&http.Transport{
			DialContext: dialContext,
		}),
	}
	resp, err := client.Do(req)
	if err != nil {
		return res, traces.RecordError(span, fmt.Errorf("request failed: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("received error response: %s", resp.Status)
	}
	if _, ok := any(&res).(*empty); ok {
		return res, nil
	}

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return res, traces.RecordError(span, fmt.Errorf("failed to decode response: %w", err))
	}
	return res, nil
}
