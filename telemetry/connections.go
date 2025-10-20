package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/getlantern/radiance/vpn/ipc"
)

// harvestConnectionMetrics periodically polls the number of active connections and their total
// upload and download bytes, setting the corresponding OpenTelemetry metrics. It returns a function
// that can be called to stop the polling.
func harvestConnectionMetrics(pollInterval time.Duration) func() {
	ticker := time.NewTicker(pollInterval)
	meter := otel.Meter("github.com/getlantern/radiance/metrics")
	currentActiveConnections, err := meter.Int64Counter("current_active_connections", metric.WithDescription("Current number of active connections"))
	if err != nil {
		slog.Warn("failed to create current_active_connections metric", slog.Any("error", err))
	}
	connectionDuration, err := meter.Float64Histogram("connection_duration_seconds", metric.WithDescription("Duration of connections in seconds"), metric.WithUnit("s"))
	if err != nil {
		slog.Warn("failed to create connection_duration_seconds metric", slog.Any("error", err))
	}
	downlinkBytes, err := meter.Int64Counter("downlink_bytes", metric.WithDescription("Total downlink bytes across all connections"), metric.WithUnit("By"))
	if err != nil {
		slog.Warn("failed to create downlink_bytes metric", slog.Any("error", err))
	}
	uplinkBytes, err := meter.Int64Counter("uplink_bytes", metric.WithDescription("Total uplink bytes across all connections"), metric.WithUnit("By"))
	if err != nil {
		slog.Warn("failed to create uplink_bytes metric", slog.Any("error", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		seenConnections := make(map[string]bool)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				slog.Debug("polling connections for metrics", slog.Int("seen_connections", len(seenConnections)), slog.Duration("poll_interval", pollInterval))
				vpnStatus, err := ipc.GetStatus(ctx)
				if err != nil {
					slog.Warn("failed to get service status", "error", err)
				}
				if vpnStatus != ipc.StatusRunning {
					continue
				}
				conns, err := ipc.GetConnections(ctx)
				if err != nil {
					slog.Warn("failed to retrieve connections", slog.Any("error", err))
					continue
				}

				for _, c := range conns {
					attributes := attribute.NewSet(
						attribute.String("from_outbound", c.FromOutbound),
						attribute.String("outbound_name", c.Outbound),
						attribute.String("inbound", c.Inbound),
						attribute.String("network", c.Network),
						attribute.String("protocol", c.Protocol),
						attribute.Int("ip_version", c.IPVersion),
						attribute.String("rule", c.Rule),
						attribute.StringSlice("chain_list", c.ChainList),
					)

					active, seen := seenConnections[c.ID]

					// not collecting duration of active connections
					if c.ClosedAt == 0 && !seen {
						seenConnections[c.ID] = true
						currentActiveConnections.Add(ctx, 1, metric.WithAttributeSet(attributes))
						continue
					}

					// already registered this closed connection
					if seen && !active {
						continue
					}

					seenConnections[c.ID] = false
					duration := float64(c.ClosedAt - c.CreatedAt)
					if duration > 0 {
						connectionDuration.Record(ctx, duration/1000, metric.WithAttributeSet(attributes))
					}

					downlinkBytes.Add(ctx, c.Downlink, metric.WithAttributeSet(attributes))
					uplinkBytes.Add(ctx, c.Uplink, metric.WithAttributeSet(attributes))
				}
			}
		}
	}()
	return cancel
}
