//go:build android || ios || (darwin && !standalone)

package ipc

import (
	"context"
	"encoding/json"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/peer"
	"github.com/getlantern/radiance/unbounded"
	"github.com/getlantern/radiance/vpn"
)

// AutoSelectedEvents streams auto-selection changes. Blocks until ctx is cancelled.
func (c *Client) AutoSelectedEvents(ctx context.Context, handler func(vpn.AutoSelectedEvent)) error {
	// The bus subscription is not redundant with SSE: when the tunnel process is down,
	// the client's fallback LocalBackend emits these events on the in-process bus.
	events.SubscribeContext(ctx, handler)
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, serverAutoSelectedEventsEndpoint, func(data []byte) {
		var evt vpn.AutoSelectedEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// URLTestEvents streams URL-test completion notifications. An event is sent
// only when a test run produced usable latency results, for either the offline
// pre-warm run or the live auto-select probe. Blocks until ctx is cancelled.
func (c *Client) URLTestEvents(ctx context.Context, handler func(vpn.URLTestCompleteEvent)) error {
	// The bus subscription is not redundant with SSE: when the tunnel process is down,
	// the client's fallback LocalBackend emits these events on the in-process bus.
	events.SubscribeContext(ctx, handler)
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, serverURLTestEventsEndpoint, func(data []byte) {
		var evt vpn.URLTestCompleteEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// ConfigEvents streams config-updated notifications. Each event signals that
// config changed and carries no payload. Blocks until ctx is cancelled.
func (c *Client) ConfigEvents(ctx context.Context, handler func()) error {
	// The bus subscription is not redundant with SSE: when the tunnel process is down,
	// the client's fallback LocalBackend emits NewConfigEvents on the in-process bus.
	events.SubscribeContext(ctx, func(config.NewConfigEvent) { handler() })
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, configEventsEndpoint, func([]byte) { handler() })
}

// VPNStatusEvents streams VPN status changes. Blocks until ctx is cancelled.
func (c *Client) VPNStatusEvents(ctx context.Context, handler func(vpn.StatusUpdateEvent)) error {
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, vpnStatusEventsEndpoint, func(data []byte) {
		var evt vpn.StatusUpdateEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// DataCapStream streams data-cap updates while the VPN is connected. Blocks until ctx is cancelled.
func (c *Client) DataCapStream(ctx context.Context, handler func(account.DataCapInfo)) error {
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.dataCapStream(ctx, handler)
}

// PeerStatusEvents streams peer-share lifecycle status changes. Frames may
// arrive out of order — a delayed frame can follow a newer one — so do not
// assume monotonic ordering. Blocks until ctx is cancelled.
func (c *Client) PeerStatusEvents(ctx context.Context, handler func(peer.StatusEvent)) error {
	events.SubscribeContext(ctx, handler)
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, peerStatusEventsEndpoint, func(data []byte) {
		var evt peer.StatusEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// PeerConnectionEvents streams accept/close events for the local samizdat-in
// inbound. Blocks until ctx is cancelled.
func (c *Client) PeerConnectionEvents(ctx context.Context, handler func(peer.ConnectionEvent)) error {
	events.SubscribeContext(ctx, handler)
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, peerConnectionEventsEndpoint, func(data []byte) {
		var evt peer.ConnectionEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// UnboundedConnectionEvents streams accept/close events for the local broflake
// widget proxy ("Unbounded" / Basic mode). Blocks until ctx is cancelled.
func (c *Client) UnboundedConnectionEvents(ctx context.Context, handler func(unbounded.ConnectionEvent)) error {
	events.SubscribeContext(ctx, handler)
	if c.localOnly {
		<-ctx.Done()
		return ctx.Err()
	}
	return c.sseRetryLoop(ctx, unboundedConnectionEventsEndpoint, func(data []byte) {
		var evt unbounded.ConnectionEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}
