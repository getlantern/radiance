package telemetry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/radiance/vpn"
)

const helperTag = "helper-0123456789ab"

func TestSpanExportRedactsHelperTags(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(newRedactingExporter(mem)))
	_, span := tp.Tracer("test").Start(context.Background(), "dial "+helperTag)
	span.SetAttributes(
		attribute.String("outbound", helperTag),
		attribute.StringSlice("chain", []string{"auto", helperTag}),
		attribute.Int("latency_ms", 7),
	)
	span.AddEvent("url_test_result", trace.WithAttributes(attribute.String("outbound", helperTag)))
	span.RecordError(errors.New("outbound/unbounded[" + helperTag + "]: dial failed"))
	span.SetStatus(codes.Error, "outbound/unbounded["+helperTag+"]: dial failed")
	span.End()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	spans := mem.GetSpans()
	require.Len(t, spans, 1)
	s := spans[0]
	assert.Equal(t, "dial helper", s.Name)
	for _, kv := range s.Attributes {
		assert.NotContains(t, kv.Value.Emit(), helperTag, string(kv.Key))
	}
	assert.Contains(t, s.Attributes, attribute.Int("latency_ms", 7), "other attributes are kept")
	require.Len(t, s.Events, 2)
	for _, ev := range s.Events {
		for _, kv := range ev.Attributes {
			assert.NotContains(t, kv.Value.Emit(), helperTag, ev.Name+"."+string(kv.Key))
		}
	}
	assert.Equal(t, "outbound/unbounded[helper]: dial failed", s.Status.Description)
}

func TestConnMetricAttributesRedactHelperTags(t *testing.T) {
	set := newConnAttributeSet(vpn.ConnAttrs{
		FromOutbound: helperTag,
		Outbound:     helperTag,
		Rule:         "final => " + helperTag,
		ChainList:    []string{"auto", helperTag},
	})
	for _, kv := range set.ToSlice() {
		assert.NotContains(t, kv.Value.Emit(), helperTag, string(kv.Key))
	}
	v, _ := set.Value("outbound_name")
	assert.Equal(t, "helper", v.AsString())
}
