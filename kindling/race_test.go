package kindling

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getlantern/kindling"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRecordRace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	ctx, span := tp.Tracer("test").Start(context.Background(), "config_fetcher.send")
	recordRace(ctx, kindling.Race{
		Winner: "smart",
		Attempts: []kindling.Attempt{
			{Transport: "smart", Outcome: kindling.OutcomeWon, StatusCode: 200, Elapsed: 700 * time.Millisecond},
			{Transport: "domainfront", Outcome: kindling.OutcomeRetryStatus, StatusCode: 403, Elapsed: 300 * time.Millisecond},
			{Transport: "amp", Outcome: kindling.OutcomeConnectFailed, Err: errors.New("dial: blocked")},
		},
	})
	span.End()

	spans := rec.Ended()
	require.Len(t, spans, 1)
	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range spans[0].Attributes() {
		attrs[kv.Key] = kv.Value
	}
	assert.Equal(t, "smart", attrs["kindling.winner"].AsString())
	assert.Equal(t, "won", attrs["kindling.smart.outcome"].AsString())
	assert.Equal(t, int64(700), attrs["kindling.smart.elapsed_ms"].AsInt64())
	assert.Equal(t, "retry_status", attrs["kindling.domainfront.outcome"].AsString())
	assert.Equal(t, int64(403), attrs["kindling.domainfront.status"].AsInt64())
	assert.Equal(t, "connect_failed", attrs["kindling.amp.outcome"].AsString())
	_, hasStatus := attrs["kindling.amp.status"]
	assert.False(t, hasStatus, "no status without a response")

	events := spans[0].Events()
	require.Len(t, events, 3)
	var ampErr string
	for _, e := range events {
		assert.Equal(t, "kindling.attempt", e.Name)
		for _, kv := range e.Attributes {
			if kv.Key == "kindling.error" {
				ampErr = kv.Value.AsString()
			}
		}
	}
	assert.Equal(t, "dial: blocked", ampErr)
}

func TestRecordRaceWithoutSpan(t *testing.T) {
	recordRace(context.Background(), kindling.Race{Winner: "smart", Attempts: []kindling.Attempt{{Transport: "smart"}}})
}
