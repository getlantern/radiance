package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/getlantern/radiance/vpn"
)

// connEventBuffer sizes the channel between the data-path push and the single recording goroutine.
// Sends are non-blocking; on overflow the event is dropped and counted rather than stalling a
// connection's open or close.
const connEventBuffer = 4096

type connEventKind uint8

const (
	connOpened connEventKind = iota
	connClosed
)

type connEvent struct {
	kind  connEventKind
	attrs vpn.ConnAttrs
	close vpn.ConnClose
}

// connObserver records connection metrics from open/close pushes. OnOpen/OnClose run on the
// connection's own goroutine and only enqueue; a single run goroutine does the OpenTelemetry work.
type connObserver struct {
	events chan connEvent

	activeConnections  metric.Int64UpDownCounter
	connectionDuration metric.Float64Histogram
	downlinkBytes      metric.Int64Counter
	uplinkBytes        metric.Int64Counter
	droppedEvents      metric.Int64Counter
}

// StartConnectionMetrics builds the connection metric instruments and starts the goroutine that
// records them, returning an observer to attach to the VPN client. Recording stops when ctx is
// canceled.
func StartConnectionMetrics(ctx context.Context) (vpn.ConnObserver, error) {
	meter := otel.Meter("github.com/getlantern/radiance/metrics")
	active, err := meter.Int64UpDownCounter(
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
	observer := &connObserver{
		events:             make(chan connEvent, connEventBuffer),
		activeConnections:  active,
		connectionDuration: dur,
		downlinkBytes:      down,
		uplinkBytes:        up,
		droppedEvents:      dropped,
	}
	go observer.run(ctx)
	return observer, nil
}

func (o *connObserver) OnOpen(attrs vpn.ConnAttrs) {
	o.enqueue(connEvent{kind: connOpened, attrs: attrs})
}

func (o *connObserver) OnClose(closeEvent vpn.ConnClose) {
	o.enqueue(connEvent{kind: connClosed, close: closeEvent})
}

func (o *connObserver) enqueue(event connEvent) {
	select {
	case o.events <- event:
	default:
		o.droppedEvents.Add(context.Background(), 1)
	}
}

func (o *connObserver) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			o.drain(context.Background())
			slog.Debug("Stopped connection metrics recording")
			return
		case event := <-o.events:
			o.record(ctx, event)
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

func (o *connObserver) record(ctx context.Context, event connEvent) {
	switch event.kind {
	case connOpened:
		attrs := metric.WithAttributeSet(newConnAttributeSet(event.attrs))
		o.activeConnections.Add(ctx, 1, attrs)

	case connClosed:
		attrs := metric.WithAttributeSet(newConnAttributeSet(event.close.ConnAttrs))
		o.activeConnections.Add(ctx, -1, attrs)

		if event.close.DurationSeconds > 0 {
			o.connectionDuration.Record(ctx, event.close.DurationSeconds, attrs)
		}
		o.downlinkBytes.Add(ctx, event.close.Downlink, attrs)
		o.uplinkBytes.Add(ctx, event.close.Uplink, attrs)
	}
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
