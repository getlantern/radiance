package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/getlantern/radiance/vpn/client"
	"github.com/sagernet/sing-box/experimental/libbox"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func connections() (*libbox.Connections, error) {
	res, err := client.SendCmd(libbox.CommandConnections)
	if err != nil {
		return nil, fmt.Errorf("failed to get connections: %w", err)
	}
	if res.Connections == nil {
		return nil, errors.New("no connections found")
	}
	res.Connections.FilterState(libbox.ConnectionStateAll)
	res.Connections.SortByDate()
	return res.Connections, nil
}

// HarvestConnectionMetrics periodically polls the number of active connections and their total
// upload and download bytes, setting the corresponding OpenTelemetry metrics. It returns a function
// that can be called to stop the polling.
func HarvestConnectionMetrics(pollInterval time.Duration) func() {
	ticker := time.NewTicker(pollInterval)
	meter := otel.Meter("github.com/getlantern/radiance/metrics")
	currentActiveConnections, _ := meter.Int64Counter("current_active_connections", metric.WithDescription("Current number of active connections"))
	connectionDuration, _ := meter.Float64Histogram("connection_duration_seconds", metric.WithDescription("Duration of connections in seconds"), metric.WithUnit("s"))
	downlinkBytes, _ := meter.Int64Counter("downlink_bytes", metric.WithDescription("Total downlink bytes across all connections"), metric.WithUnit("By"))
	uplinkBytes, _ := meter.Int64Counter("uplink_bytes", metric.WithDescription("Total uplink bytes across all connections"), metric.WithUnit("By"))
	go func() {
		seenConnections := make(map[string]bool)
		for range ticker.C {
			conns, err := connections()
			if err != nil {
				slog.Warn("failed to retrieve connections", "error", err)
				continue
			}

			for conns.Iterator().HasNext() {
				c := conns.Iterator().Next()

				attributes := attribute.NewSet(
					attribute.String("from_outbound", c.FromOutbound),
					attribute.String("outbound_name", c.Outbound),
					attribute.String("outbound_type", c.OutboundType),
					attribute.String("inbound", c.Inbound),
					attribute.String("inbound_type", c.InboundType),
					attribute.String("network", c.Network),
					attribute.String("protocol", c.Protocol),
					attribute.Int("ip_version", int(c.IPVersion)),
					attribute.String("rule", c.Rule),
					attribute.StringSlice("chain_list", c.ChainList),
				)

				active, seen := seenConnections[c.ID]
				seenConnections[c.ID] = c.ClosedAt == 0

				// not collecting duration of active connections
				if c.ClosedAt == 0 && !seen {
					currentActiveConnections.Add(context.Background(), 1, metric.WithAttributeSet(attributes))
					continue
				}

				// already registered this closed connection
				if seen && !active {
					continue
				}

				duration := float64(c.ClosedAt - c.CreatedAt)
				if duration > 0 {
					connectionDuration.Record(context.Background(), duration/1000, metric.WithAttributeSet(attributes))
				}

				downlinkBytes.Add(context.Background(), int64(c.Downlink), metric.WithAttributeSet(attributes))
				uplinkBytes.Add(context.Background(), int64(c.Uplink), metric.WithAttributeSet(attributes))
			}
		}
	}()
	return ticker.Stop
}
