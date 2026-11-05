package backend

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/getlantern/lantern-box/protocol/samizdatreverse"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/peer"
	"github.com/getlantern/radiance/vpn"
)

// helperReverseFeature is the config feature that lets this device serve its
// paired helpers as a reverse inviter.
const helperReverseFeature = "helper_reverse"

// reverseStopTimeout bounds how long the gate waits for the reverse client's
// teardown; the teardown runs to its own deadline either way.
const reverseStopTimeout = 10 * time.Second

// reverseUnsupported reports platforms that must never run the reverse client.
// It is a var solely so tests can fake it, like peerShareUnsupported.
var reverseUnsupported = common.IsIOS

// reverseController is the subset of *peer.ReverseClient that LocalBackend
// needs.
type reverseController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context)
	NetworkChanged()
	PairingsChanged()
}

func newReverseClient(deviceID string, keys *helper.PairingKeys, box peer.ReverseBox) *peer.ReverseClient {
	return peer.NewReverseClient(peer.ReverseConfig{
		API:  helper.NewReverseAPI(kindling.HTTPClient(), common.GetBaseURL(), deviceID),
		Keys: keys,
		Box:  box,
	})
}

// useReverseClient makes c the backend's reverse client and points pairing key
// changes at it.
func (r *LocalBackend) useReverseClient(c reverseController) {
	r.reverseClient = c
	r.helperPairingKeys.SetOnChange(c.PairingsChanged)
}

// startReverse runs the reverse client while the VPN is connected and the
// config enables helperReverseFeature, until stopReverse or r.ctx ends.
func (r *LocalBackend) startReverse() {
	if r.reverseClient == nil || reverseUnsupported() {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	g := &reverseGate{
		client: r.reverseClient,
		kick:   make(chan struct{}, 1),
		cancel: cancel,
		exited: make(chan struct{}),
	}
	if prev := r.reverseGate.Swap(g); prev != nil {
		prev.stop()
	}
	events.SubscribeContext(ctx, func(evt vpn.StatusUpdateEvent) { g.setVPNStatus(evt.Status) })
	events.SubscribeContext(ctx, func(evt config.NewConfigEvent) {
		if evt.New != nil {
			g.setFeature(evt.New.Features[helperReverseFeature])
		}
	})
	// Seeded after subscribing, so a change in between is not lost.
	if r.vpnClient != nil {
		g.setVPNStatus(r.vpnClient.Status())
	}
	if cfg, err := r.confHandler.GetConfig(); err == nil && cfg != nil {
		g.setFeature(cfg.Features[helperReverseFeature])
	}
	go g.run(ctx)
}

// stopReverse stops the reverse client and waits, at most about
// reverseStopTimeout, for its teardown.
func (r *LocalBackend) stopReverse() {
	if g := r.reverseGate.Swap(nil); g != nil {
		g.stop()
	}
}

func (r *LocalBackend) reverseNetworkChanged() {
	if r.reverseClient != nil {
		r.reverseClient.NetworkChanged()
	}
}

// reverseGate starts and stops a reverse client on one goroutine, so the event
// and IPC paths that report its inputs never wait on it.
type reverseGate struct {
	client reverseController
	kick   chan struct{}
	cancel context.CancelFunc
	exited chan struct{}

	mu        sync.Mutex
	vpnUp     bool
	featureOn bool
}

func (g *reverseGate) setVPNStatus(s vpn.VPNStatus) {
	switch s {
	case vpn.Connected:
		g.set(&g.vpnUp, true)
	case vpn.Connecting, vpn.Restarting:
		// A restart keeps the client: the new tunnel restores its portal and
		// outbounds. A connect follows a disconnect, which already stopped it.
	default:
		g.set(&g.vpnUp, false)
	}
}

func (g *reverseGate) setFeature(on bool) { g.set(&g.featureOn, on) }

func (g *reverseGate) set(field *bool, v bool) {
	g.mu.Lock()
	*field = v
	g.mu.Unlock()
	select {
	case g.kick <- struct{}{}:
	default:
	}
}

func (g *reverseGate) wanted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.vpnUp && g.featureOn
}

func (g *reverseGate) run(ctx context.Context) {
	defer close(g.exited)
	running := false
	for {
		select {
		case <-ctx.Done():
			if running {
				g.stopClient()
			}
			return
		case <-g.kick:
		}
		want := g.wanted()
		switch {
		case want && !running:
			if err := g.client.Start(ctx); err != nil {
				slog.Warn("Failed to start the reverse client", "error", err)
				continue
			}
			running = true
		case !want && running:
			g.stopClient()
			running = false
		}
	}
}

func (g *reverseGate) stopClient() {
	ctx, cancel := context.WithTimeout(context.Background(), reverseStopTimeout)
	defer cancel()
	g.client.Stop(ctx)
}

func (g *reverseGate) stop() {
	g.cancel()
	<-g.exited
}

// reverseBox is the VPN client as the reverse client's box. Changing the
// reverse outbounds can withdraw the friend a selection names, so each change
// re-checks the selection under r.selectionMu, as config updates do.
type reverseBox struct{ r *LocalBackend }

func (b reverseBox) SetReversePortal(cfg *samizdatreverse.PortalConfig, masqueradeDomain string) error {
	b.r.selectionMu.Lock()
	defer b.r.selectionMu.Unlock()
	err := b.r.vpnClient.SetReversePortal(cfg, masqueradeDomain)
	b.reconcileSelectionLocked()
	return err
}

func (b reverseBox) SetReverseOutbounds(tags []string) error {
	b.r.selectionMu.Lock()
	defer b.r.selectionMu.Unlock()
	err := b.r.vpnClient.SetReverseOutbounds(tags)
	b.reconcileSelectionLocked()
	return err
}

func (b reverseBox) RebindReversePortal() error { return b.r.vpnClient.RebindReversePortal() }

func (b reverseBox) reconcileSelectionLocked() {
	// After Close a replacement backend may own the selection.
	if !b.r.selectionClosed {
		b.r.clearSelectedIfMissingLocked()
	}
}
