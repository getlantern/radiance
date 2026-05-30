//go:build (!android && !ios && !darwin) || (darwin && standalone)

package ipc

import (
	"context"
	"encoding/json"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/peer"
	"github.com/getlantern/radiance/unbounded"
	"github.com/getlantern/radiance/vpn"
)

// AutoSelectedEvents streams auto-selection changes. Blocks until ctx is cancelled.
func (c *Client) AutoSelectedEvents(ctx context.Context, handler func(vpn.AutoSelectedEvent)) error {
	return c.sseRetryLoop(ctx, serverAutoSelectedEventsEndpoint, func(data []byte) {
		var evt vpn.AutoSelectedEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// ConfigEvents streams config-updated notifications. Payloads are empty — callers should treat each
// call as a "refresh" signal. Blocks until ctx is cancelled.
func (c *Client) ConfigEvents(ctx context.Context, handler func()) error {
	return c.sseRetryLoop(ctx, configEventsEndpoint, func([]byte) { handler() })
}

// VPNStatusEvents streams VPN status changes. Blocks until ctx is cancelled.
func (c *Client) VPNStatusEvents(ctx context.Context, handler func(vpn.StatusUpdateEvent)) error {
	return c.sseRetryLoop(ctx, vpnStatusEventsEndpoint, func(data []byte) {
		var evt vpn.StatusUpdateEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// DataCapStream streams data-cap updates while the VPN is connected. Blocks until ctx is cancelled.
func (c *Client) DataCapStream(ctx context.Context, handler func(account.DataCapInfo)) error {
	return c.dataCapStream(ctx, handler)
}

// PeerStatusEvents streams peer-share lifecycle phase changes (mapping_port
// → registering → verifying → serving on Start, stopping → idle on Stop,
// error on failure). Each frame is a peer.StatusEvent JSON whose .Status
// is the live snapshot at the moment the event fired — consumers SHOULD
// re-render on every frame rather than diffing, since events.Emit's
// per-callback goroutine can land Start phases out of order. Blocks until
// ctx is cancelled.
func (c *Client) PeerStatusEvents(ctx context.Context, handler func(peer.StatusEvent)) error {
	return c.sseRetryLoop(ctx, peerStatusEventsEndpoint, func(data []byte) {
		var evt peer.StatusEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// PeerConnectionEvents streams accept/close events for the local
// samizdat-in inbound. State is +1 on accept and -1 on close; Source
// is the remote "ip:port" string for geo-lookup / abuse attribution.
// Blocks until ctx is cancelled.
//
// Why this exists alongside events.Subscribe[peer.ConnectionEvent]:
// the events package's globals are process-scoped, so a subscriber in
// Liblantern can't see emits in lanternd. The SSE path bridges them.
func (c *Client) PeerConnectionEvents(ctx context.Context, handler func(peer.ConnectionEvent)) error {
	return c.sseRetryLoop(ctx, peerConnectionEventsEndpoint, func(data []byte) {
		var evt peer.ConnectionEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}

// UnboundedConnectionEvents streams accept/close events for the
// local broflake widget proxy ("Unbounded" / Basic mode). The JSON
// shape matches peer.ConnectionEvent but the Go type is distinct —
// in-process subscribers must subscribe to both event types separately
// to see all peer activity. State is +1 on consumer accept, -1 on
// close; Source is the consumer's IP if broflake exposes it,
// otherwise empty. Blocks until ctx is cancelled.
func (c *Client) UnboundedConnectionEvents(ctx context.Context, handler func(unbounded.ConnectionEvent)) error {
	return c.sseRetryLoop(ctx, unboundedConnectionEventsEndpoint, func(data []byte) {
		var evt unbounded.ConnectionEvent
		if err := json.Unmarshal(data, &evt); err == nil {
			handler(evt)
		}
	})
}
