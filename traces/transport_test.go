package traces

import (
	"context"
	"net/http"
	"strings"
	"testing"

	bfcommon "github.com/getlantern/broflake/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type okTransport struct{}

func (okTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// Room tickets are bearer credentials carrying a pairing ID; no part of them
// may reach a trace.
func TestHeaderAnnotationNeverRecordsRoomTickets(t *testing.T) {
	require.Equal(t, roomTicketsHeader, strings.ToLower(bfcommon.RoomTicketsHeader))
	const ticket = "eyJ2IjoxLCJrIjoiYzUxYzUwNzUiLCJwIjoiNmYxYzJiMGUifQ.c2lnbmF0dXJl"
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "subscribe")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://freddie.example/v1/signal", nil)
	require.NoError(t, err)
	req.Header.Set(bfcommon.RoomTicketsHeader, ticket+","+ticket)
	req.Header.Set("X-Other", "kept")
	_, err = NewHeaderAnnotatingRoundTripper(okTransport{}).RoundTrip(req)
	require.NoError(t, err)
	span.End()

	spans := mem.GetSpans()
	require.Len(t, spans, 1)
	var keys []string
	for _, kv := range spans[0].Attributes {
		keys = append(keys, string(kv.Key))
		assert.NotContains(t, kv.Value.Emit(), ticket[:4], string(kv.Key))
	}
	assert.NotContains(t, keys, "http.request.header.x-bf-room-tickets")
	assert.Contains(t, keys, "http.request.header.x-other")
}
