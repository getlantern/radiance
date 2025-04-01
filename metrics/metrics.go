package metrics

import (
	"github.com/sagernet/sing-box/adapter"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

type metricsManager struct {
	meter         metric.Meter
	bytesSent     metric.Int64Counter
	bytesReceived metric.Int64Counter
	duration      metric.Int64Histogram
	conns         metric.Int64UpDownCounter
}

var metrics = newMetricsManager()

func newMetricsManager() *metricsManager {
	meter := otel.GetMeterProvider().Meter("radiance")
	bytesSent, err := meter.Int64Counter("radiance.bytes_sent", metric.WithDescription("Bytes sent"))
	if err != nil {
		bytesSent = &noop.Int64Counter{}
	}
	bytesReceived, err := meter.Int64Counter("radiance.bytes_received", metric.WithDescription("Bytes received"))
	if err != nil {
		bytesReceived = &noop.Int64Counter{}
	}

	// Track connection duration.
	duration, err := meter.Int64Histogram("radiance.connection_duration", metric.WithDescription("Connection duration"))
	if err != nil {
		duration = &noop.Int64Histogram{}
	}

	// Track the number of connections.
	conns, err := meter.Int64UpDownCounter("radiance.connections", metric.WithDescription("Number of connections"))
	if err != nil {
		conns = &noop.Int64UpDownCounter{}
	}
	return &metricsManager{
		meter:         meter,
		bytesSent:     bytesSent,
		bytesReceived: bytesReceived,
		duration:      duration,
		conns:         conns,
	}
}

func metadataToAttributes(metadata *adapter.InboundContext) []attribute.KeyValue {
	// Convert metadata to attributes
	return []attribute.KeyValue{
		attribute.String("proxy_ip", metadata.Destination.IPAddr().String()),
		attribute.String("protocol", metadata.Protocol),
		attribute.String("user", metadata.User),
		attribute.String("inbound", metadata.Inbound),
		attribute.String("outbound", metadata.Outbound),
		attribute.String("client", metadata.Client),
		attribute.String("domain", metadata.Domain),
	}
}
