package vpn

import (
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"

	"github.com/getlantern/radiance/events"
)

// URLTestHistoryStorage is an alias for the sing-box adapter interface.
type URLTestHistoryStorage = adapter.URLTestHistoryStorage

// StatusUpdateEvent is emitted when the VPN status changes.
type StatusUpdateEvent struct {
	events.Event
	Status VPNStatus `json:"status"`
	Error  string    `json:"error,omitempty"`
}

// Selector is helper interface to check if an outbound is a selector or wrapper of selector.
type Selector interface {
	adapter.OutboundGroup
	SelectOutbound(tag string) bool
}

type OutboundGroup struct {
	Tag       string      `json:"tag"`
	Type      string      `json:"type"`
	Selected  string      `json:"selected"`
	Outbounds []Outbounds `json:"outbounds"`
}

type Outbounds struct {
	Tag  string `json:"tag"`
	Type string `json:"type"`
}

// ThroughputSnapshot is the most recent throughput sample for the tunnel.
type ThroughputSnapshot struct {
	Global            Throughput            `json:"global"`
	PerOutbound       map[string]Throughput `json:"per_outbound"`
	ActiveConnections int                   `json:"active_connections"`
	ActivePerOutbound map[string]int        `json:"active_per_outbound"`
}

type Connection struct {
	ID           string   `json:"id"`
	Inbound      string   `json:"inbound"`
	IPVersion    int      `json:"ip_version"`
	Network      string   `json:"network"`
	Source       string   `json:"source"`
	Destination  string   `json:"destination"`
	Domain       string   `json:"domain,omitempty"`
	Protocol     string   `json:"protocol,omitempty"`
	FromOutbound string   `json:"from_outbound,omitempty"`
	CreatedAt    int64    `json:"created_at"`
	ClosedAt     int64    `json:"closed_at,omitempty"`
	Uplink       int64    `json:"uplink"`
	Downlink     int64    `json:"downlink"`
	Rule         string   `json:"rule,omitempty"`
	Outbound     string   `json:"outbound"`
	ChainList    []string `json:"chain,omitempty"`
}

// NewConnection creates a Connection from tracker metadata.
func newConnection(metadata trafficontrol.TrackerMetadata) Connection {
	var rule string
	if metadata.Rule != nil {
		rule = metadata.Rule.String() + " => " + metadata.Rule.Action().String()
	}
	var closedAt int64
	if !metadata.ClosedAt.IsZero() {
		closedAt = metadata.ClosedAt.UnixMilli()
	}
	md := metadata.Metadata
	return Connection{
		ID:           metadata.ID.String(),
		Inbound:      md.InboundType + "/" + md.Inbound,
		IPVersion:    int(md.IPVersion),
		Network:      md.Network,
		Source:       md.Source.String(),
		Destination:  md.Destination.String(),
		Domain:       md.Domain,
		Protocol:     md.Protocol,
		FromOutbound: md.Outbound,
		CreatedAt:    metadata.CreatedAt.UnixMilli(),
		ClosedAt:     closedAt,
		Uplink:       metadata.Upload.Load(),
		Downlink:     metadata.Download.Load(),
		Rule:         rule,
		Outbound:     metadata.OutboundType + "/" + metadata.Outbound,
		ChainList:    metadata.Chain,
	}
}
