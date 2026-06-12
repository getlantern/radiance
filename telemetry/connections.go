package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/getlantern/radiance/vpn"
)

// connEventBuffer sizes the channel used for buffered ConnClose events.
// Sends are non-blocking; on overflow the event is dropped and counted rather than stalling
// a connection close.
const connEventBuffer = 4096

type connObserver struct {
	events chan vpn.ConnClose

	connectionDuration metric.Float64Histogram
	downlinkBytes      metric.Int64Counter
	uplinkBytes        metric.Int64Counter
	droppedEvents      metric.Int64Counter

	activeConnectionsReg metric.Registration
}

// StartConnectionMetrics builds the connection metric instruments and starts the goroutine that
// records them, returning an observer to attach to the VPN client. Recording stops when ctx
// is canceled.
func StartConnectionMetrics(ctx context.Context, activeConnections func() int64) (vpn.ConnObserver, error) {
	meter := otel.Meter("github.com/getlantern/radiance/metrics")

	active, err := meter.Int64ObservableGauge(
		"current_active_connections",
		metric.WithDescription("Current number of active connections"),
	)
	if err != nil {
		return nil, err
	}
	dur, err := meter.Float64Histogram(
		"connection_duration_seconds",
		metric.WithDescription("Duration of connections in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}
	down, err := meter.Int64Counter(
		"downlink_bytes",
		metric.WithDescription("Total downlink bytes across all connections"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}
	up, err := meter.Int64Counter(
		"uplink_bytes",
		metric.WithDescription("Total uplink bytes across all connections"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}
	dropped, err := meter.Int64Counter(
		"connection_metric_events_dropped",
		metric.WithDescription("Connection metric events dropped because the recording buffer was full"),
	)
	if err != nil {
		return nil, err
	}
	activeReg, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(active, activeConnections())
		return nil
	}, active)
	if err != nil {
		return nil, err
	}

	observer := &connObserver{
		events:               make(chan vpn.ConnClose, connEventBuffer),
		connectionDuration:   dur,
		downlinkBytes:        down,
		uplinkBytes:          up,
		droppedEvents:        dropped,
		activeConnectionsReg: activeReg,
	}
	go observer.run(ctx)

	return observer, nil
}

func (o *connObserver) OnClose(closeEvent vpn.ConnClose) {
	select {
	case o.events <- closeEvent:
	default:
		o.droppedEvents.Add(context.Background(), 1)
	}
}

func (o *connObserver) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if err := o.activeConnectionsReg.Unregister(); err != nil {
				slog.Debug("Failed to unregister active-connections gauge", "error", err)
			}
			o.drain(context.Background())
			slog.Debug("Stopped connection metrics recording")
			return
		case closeEvent := <-o.events:
			o.record(ctx, closeEvent)
		}
	}
}

func (o *connObserver) drain(ctx context.Context) {
	for {
		select {
		case event := <-o.events:
			o.record(ctx, event)
		default:
			return
		}
	}
}

func (o *connObserver) record(ctx context.Context, closeEvent vpn.ConnClose) {
	attrs := metric.WithAttributeSet(newConnAttributeSet(closeEvent.ConnAttrs))
	if closeEvent.DurationSeconds > 0 {
		o.connectionDuration.Record(ctx, closeEvent.DurationSeconds, attrs)
	}
	o.downlinkBytes.Add(ctx, closeEvent.Downlink, attrs)
	o.uplinkBytes.Add(ctx, closeEvent.Uplink, attrs)
}

func newConnAttributeSet(attrs vpn.ConnAttrs) attribute.Set {
	return attribute.NewSet(
		attribute.String("from_outbound", attrs.FromOutbound),
		attribute.String("outbound_name", attrs.Outbound),
		attribute.String("inbound", attrs.Inbound),
		attribute.String("network", attrs.Network),
		attribute.String("protocol", attrs.Protocol),
		attribute.Int("ip_version", attrs.IPVersion),
		attribute.String("rule", attrs.Rule),
		attribute.StringSlice("chain_list", attrs.ChainList),
	)
}
