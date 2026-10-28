package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/getlantern/semconv"
)

func TestParentContext(t *testing.T) {
	t.Setenv("TRACEPARENT", "00-323cc51c1b2848fdab828f68b76ff811-0120f713aaa5e6f2-01")
	sc := trace.SpanContextFromContext(parentContext(context.Background()))
	require.True(t, sc.IsValid())
	assert.Equal(t, "323cc51c1b2848fdab828f68b76ff811", sc.TraceID().String())
	assert.Equal(t, "0120f713aaa5e6f2", sc.SpanID().String())
	assert.True(t, sc.IsSampled())
	assert.True(t, sc.IsRemote())
	for _, invalid := range []string{"", "garbage", "00-00000000000000000000000000000000-0120f713aaa5e6f2-01"} {
		t.Setenv("TRACEPARENT", invalid)
		assert.False(t, trace.SpanContextFromContext(parentContext(context.Background())).IsValid(), invalid)
	}
}

func TestExporterHeaders(t *testing.T) {
	assert.Empty(t, exporterHeaders(""))
	assert.Equal(t, map[string]string{"signoz-ingestion-key": "abc", "x-other": "a b", "x-plus": "a+b"},
		exporterHeaders("signoz-ingestion-key=abc, x-other=a%20b,x-plus=a+b,novalue,=empty"))
}

func TestStartTelemetryWithoutEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	flush, err := startTelemetry(context.Background())
	require.NoError(t, err)
	assert.NoError(t, flush(context.Background()))
}

func TestFetchAttributes(t *testing.T) {
	assert.Nil(t, fetchAttributes([]byte("{")))
	assert.Nil(t, fetchAttributes([]byte(`{"http_code":200}`)))
	got := fetchAttributes([]byte(`{"http_code":503,"exitcode":0,"time_total":2.345,"time_starttransfer":0.9,"remote_ip":"192.0.2.1"}`))
	assert.ElementsMatch(t, []attribute.KeyValue{
		semconv.HTTPResponseStatusCodeKey.Int(503), semconv.TesterCurlExitCodeKey.Int(0),
		semconv.TesterTotalMsKey.Int(2345), semconv.TesterFirstByteMsKey.Int(900),
		semconv.NetworkPeerAddressKey.String("192.0.2.1"),
	}, got)
	got = fetchAttributes([]byte(`{"http_code":0,"exitcode":28,"errormsg":"timeout"}`))
	assert.Contains(t, got, semconv.TesterCurlErrorKey.String("timeout"))
	assert.Contains(t, got, semconv.TesterCurlExitCodeKey.Int(28))
}
