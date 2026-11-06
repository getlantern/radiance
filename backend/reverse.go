package backend

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/peer"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

// helperReverseFeature is the config feature that lets this device serve its
// paired helpers as a reverse inviter.
const helperReverseFeature = "helper_reverse"

// reverseStopTimeout bounds how long the gate waits for the reverse client's
// teardown; the teardown runs to its own deadline either way.
const reverseStopTimeout = 10 * time.Second

// reverseUnsupported reports whether this platform must never run the reverse
// client. It is a var solely so tests can fake it.
var reverseUnsupported = common.IsIOS

// reverseController is the subset of *peer.ReverseClient that LocalBackend
// needs.
type reverseController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context)
	NetworkChanged()
	PairingsChanged()
}

func (r *LocalBackend) newReverseClient(deviceID string) *peer.ReverseClient {
	return peer.NewReverseClient(peer.ReverseConfig{
		API:    helper.NewReverseAPI(kindling.HTTPClient(), common.GetBaseURL(), deviceID),
		Keys:   r.helperPairingKeys,
		Box:    reverseBox{r: r},
		OnTags: r.reverseFriends.setTags,
	})
}

func (r *LocalBackend) newBridgeClient(deviceID string) *helper.BridgeClient {
	return helper.NewBridgeClient(helper.BridgeConfig{
		API:    helper.NewReverseAPI(kindling.HTTPClient(), common.GetBaseURL(), deviceID),
		Keys:   r.helperPairingKeys,
		NewBox: peer.NewBridgeBox,
		Usage:  r.helperUsage,
	})
}

// useReverseClient makes c the backend's reverse client and points pairing key
// changes at it.
func (r *LocalBackend) useReverseClient(c reverseController) {
	r.reverseClient = c
	r.helperPairingKeys.SetOnChange(r.pairingKeysChanged)
}

func (r *LocalBackend) pairingKeysChanged() {
	if r.reverseClient != nil {
		r.reverseClient.PairingsChanged()
		go r.pruneReverseFriends()
	}
	if r.bridgeClient != nil {
		r.bridgeClient.PairingsChanged()
	}
}

// pruneReverseFriends forgets the reverse friends whose inviter pairing ended,
// and with them a selection that only they kept.
func (r *LocalBackend) pruneReverseFriends() {
	if !r.reverseFriends.prune(r.helperPairingKeys.IDs(string(helperticket.RoleInviter))) {
		return
	}
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	if !r.selectionClosed {
		r.clearSelectedIfMissingLocked()
	}
}

// startReverse runs the reverse client while the VPN is connected and the
// config enables helperReverseFeature, until stopReverse or r.ctx ends.
func (r *LocalBackend) startReverse() {
	status := func() vpn.VPNStatus { return "" }
	if r.vpnClient != nil {
		status = r.vpnClient.Status
	}
	r.startReverseWith(status)
}

func (r *LocalBackend) startReverseWith(vpnStatus func() vpn.VPNStatus) {
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
	g.setVPNStatus(vpnStatus())
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
		// outbounds. Every other way out of Connected already set the VPN down.
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

// reverseBox is the VPN client as the reverse client's box. It records the
// friends of each reverse outbound set it publishes in r.reverseFriends.
type reverseBox struct{ r *LocalBackend }

func (b reverseBox) SetReversePortal(cfg *samizdatreverse.PortalConfig, masqueradeDomain string) error {
	return b.r.vpnClient.SetReversePortal(cfg, masqueradeDomain)
}

func (b reverseBox) SetReverseOutbounds(tags []string) error {
	if err := b.r.vpnClient.SetReverseOutbounds(tags); err != nil {
		return err
	}
	if len(tags) > 0 {
		b.r.reverseFriends.record(tags)
		b.r.restoreReverseSelection(tags)
	}
	return nil
}

func (b reverseBox) RebindReversePortal() error { return b.r.vpnClient.RebindReversePortal() }

// restoreReverseSelection has the tunnel select the saved friend once one of
// reverseTags is that friend's: a tunnel that came up without the friend's
// outbound fell back from it.
func (r *LocalBackend) restoreReverseSelection(reverseTags []string) {
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	if r.selectionClosed || settings.GetBool(settings.AutoConnectKey) {
		return
	}
	var saved servers.Server
	if err := settings.GetStruct(settings.SelectedServerKey, &saved); err != nil {
		return
	}
	friend, ok := vpn.ParsePersonalProxySelection(saved.Tag)
	if !ok || !slices.Contains(reverseTags, lbA.ReverseHelperTag(friend)) ||
		r.liveTunnel().SelectedPersonalProxy() == friend {
		return
	}
	if err := r.liveTunnel().SelectServer(saved.Tag); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		// The error can name the friend.
		slog.Warn("Failed to restore the saved selection once its reverse outbound returned")
	}
}
