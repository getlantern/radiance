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
	Tag       string
	Type      string
	Selected  string
	Outbounds []Outbounds
}

type Outbounds struct {
	Tag  string
	Type string
}

type Connection struct {
	ID           string
	Inbound      string
	IPVersion    int
	Network      string
	Source       string
	Destination  string
	Domain       string
	Protocol     string
	FromOutbound string
	CreatedAt    int64
	ClosedAt     int64
	Uplink       int64
	Downlink     int64
	Rule         string
	Outbound     string
	ChainList    []string
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
