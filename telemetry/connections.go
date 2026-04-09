package telemetry

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/getlantern/radiance/vpn"
)

// ConnectionSource provides access to the current VPN connections for metrics collection.
type ConnectionSource interface {
	Connections() ([]vpn.Connection, error)
}

// StartConnectionMetrics periodically polls the number of active connections and their total
// upload and download bytes, setting the corresponding OpenTelemetry metrics. It runs until the
// provided context is canceled.
func StartConnectionMetrics(ctx context.Context, src ConnectionSource, pollInterval time.Duration) {
	ticker := time.NewTicker(pollInterval)
	meter := otel.Meter("github.com/getlantern/radiance/metrics")
	currentActiveConnections, err := meter.Int64Counter("current_active_connections", metric.WithDescription("Current number of active connections"))
	if err != nil {
		slog.Warn("failed to create current_active_connections metric", slog.Any("error", err))
		return
	}
	connectionDuration, err := meter.Float64Histogram("connection_duration_seconds", metric.WithDescription("Duration of connections in seconds"), metric.WithUnit("s"))
	if err != nil {
		slog.Warn("failed to create connection_duration_seconds metric", slog.Any("error", err))
		return
	}
	downlinkBytes, err := meter.Int64Counter("downlink_bytes", metric.WithDescription("Total downlink bytes across all connections"), metric.WithUnit("By"))
	if err != nil {
		slog.Warn("failed to create downlink_bytes metric", slog.Any("error", err))
		return
	}
	uplinkBytes, err := meter.Int64Counter("uplink_bytes", metric.WithDescription("Total uplink bytes across all connections"), metric.WithUnit("By"))
	if err != nil {
		slog.Warn("failed to create uplink_bytes metric", slog.Any("error", err))
		return
	}
	go func() {
		seenConnections := make(map[string]bool)
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				slog.Debug("polling connections for metrics", slog.Int("seen_connections", len(seenConnections)), slog.Duration("poll_interval", pollInterval))
				conns, err := src.Connections()
				if err != nil {
					slog.Debug("failed to retrieve connections for metrics", slog.Any("error", err))
					continue
				}

				// Track which connections are still reported so we can prune stale entries.
				currentIDs := make(map[string]struct{}, len(conns))
				for _, c := range conns {
					currentIDs[c.ID] = struct{}{}
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

				// Remove entries for connections no longer reported by the source.
				for id := range seenConnections {
					if _, ok := currentIDs[id]; !ok {
						delete(seenConnections, id)
					}
				}
			}
		}
	}()
}
