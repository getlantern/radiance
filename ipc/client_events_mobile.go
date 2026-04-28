//go:build android || ios || (darwin && !standalone)

package ipc

import (
	"context"
	"encoding/json"

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/vpn"
)

// AutoSelectedEvents streams auto-selection changes. Blocks until ctx is cancelled.
func (c *Client) AutoSelectedEvents(ctx context.Context, handler func(vpn.AutoSelectedEvent)) error {
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

// ConfigEvents streams config-updated notifications. Payloads are empty — callers should treat each
// call as a "refresh" signal. Blocks until ctx is cancelled.
func (c *Client) ConfigEvents(ctx context.Context, handler func()) error {
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
