package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/traces"
)

const tracerName = "github.com/getlantern/radiance/vpn/ipc"

// empty is a placeholder type for requests that do not expect a response body.
type empty struct{}

// sendRequest sends an HTTP request to the specified endpoint with the given method and data.
func sendRequest[T any](ctx context.Context, method, endpoint string, data any) (T, error) {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "vpn.ipc",
		trace.WithAttributes(attribute.String("endpoint", endpoint)),
	)
	defer span.End()

	buf, err := json.Marshal(data)
	var res T
	if err != nil {
		return res, traces.RecordError(ctx, fmt.Errorf("failed to marshal payload: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL+endpoint, bytes.NewReader(buf))
	if err != nil {
		return res, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: dialContext,
		},
	}
	resp, err := client.Do(req)
	if errors.Is(err, os.ErrNotExist) {
		err = ErrIPCNotRunning
	}
	if err != nil {
		return res, traces.RecordError(ctx, fmt.Errorf("request failed: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, traces.RecordError(ctx, readErrorResponse(resp))
	}
	if _, ok := any(&res).(*empty); ok {
		return res, nil
	}

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return res, traces.RecordError(ctx, fmt.Errorf("failed to decode response: %w", err))
	}
	return res, nil
}

func readErrorResponse(resp *http.Response) error {
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read error response body: %w, status: %s", err, resp.Status)
	}
	return fmt.Errorf("%s: %s", resp.Status, buf)
}
