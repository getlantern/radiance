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

func TestSpanExportRedactsVolunteerAddresses(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(newRedactingExporter(mem)))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	_, span := tp.Tracer("test").Start(context.Background(), "connect")
	span.RecordError(errors.New("outbound/samizdat[" + helperTag + "]: dial tcp [2001:db8::7]:443"))
	span.SetStatus(codes.Error, "outbound/samizdat["+helperTag+"]: dial tcp 203.0.113.7:443")
	span.SetAttributes(attribute.String("peer", "198.51.100.9:443"))
	span.End()

	s := mem.GetSpans()[0]
	assert.Equal(t, "outbound/samizdat[helper]: dial tcp <redacted>", s.Status.Description)
	evAttrs := attribute.NewSet(s.Events[0].Attributes...)
	msg, _ := evAttrs.Value("exception.message")
	assert.Equal(t, "outbound/samizdat[helper]: dial tcp <redacted>", msg.AsString())
	assert.Contains(t, s.Attributes, attribute.String("peer", "198.51.100.9:443"), "no helper outbound named: kept")
}

func TestRedactedSpanReturnsOriginalsWhenNothingChanges(t *testing.T) {
	ro := tracetest.SpanStub{
		Name:   "clean",
		Events: []sdktrace.Event{{Name: "e", Attributes: []attribute.KeyValue{attribute.String("k", "v")}}},
		Links:  []sdktrace.Link{{Attributes: []attribute.KeyValue{attribute.String("k", "v")}}},
	}.Snapshot()
	rs := redactedSpan{ReadOnlySpan: ro}
	assert.Same(t, &ro.Events()[0], &rs.Events()[0])
	assert.Same(t, &ro.Links()[0], &rs.Links()[0])
}
