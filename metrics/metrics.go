package metrics

import (
	"go.opentelemetry.io/otel"
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
	meter := otel.GetMeterProvider().Meter("sing-box")
	bytesSent, err := meter.Int64Counter("sing_box.bytes_sent", metric.WithDescription("Bytes sent"))
	if err != nil {
		bytesSent = &noop.Int64Counter{}
	}
	bytesReceived, err := meter.Int64Counter("sing_box.bytes_received", metric.WithDescription("Bytes received"))
	if err != nil {
		bytesReceived = &noop.Int64Counter{}
	}

	// Track connection duration.
	duration, err := meter.Int64Histogram("sing_box.connection_duration", metric.WithDescription("Connection duration"))
	if err != nil {
		duration = &noop.Int64Histogram{}
	}

	// Track the number of connections.
	conns, err := meter.Int64UpDownCounter("sing_box.connections", metric.WithDescription("Number of connections"))
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
