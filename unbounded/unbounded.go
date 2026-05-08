// Package unbounded manages the broflake / Unbounded widget-proxy lifecycle.
//
// Unbounded is the WebRTC-based donor mode for Lantern's Share My Connection
// feature: the local user contributes bandwidth to censored users via short-
// lived WebRTC sessions brokered through a discovery server, without exposing
// a long-lived inbound port the way the samizdat-over-UPnP "Share My
// Connection" mode does. It's the lower-bandwidth, lower-risk, universally-
// applicable alternative to SmC — works on networks where UPnP is disabled
// or unavailable, and the peer's residential IP isn't tied to a single
// long-lived inbound listener.
//
// Three conditions must all hold for the widget proxy to actually run:
//
//  1. settings.UnboundedKey is true (local opt-in via the UI toggle)
//  2. server-side cfg.Features[UNBOUNDED] is enabled (server says go)
//  3. server-side cfg.Unbounded provides discovery + egress URLs
//
// The manager subscribes to config.NewConfigEvent and recomputes the
// running state on every config update; it also re-evaluates when
// SetEnabled flips the local toggle. Each consumer connection change
// (accept / disconnect) emits a ConnectionEvent on the radiance event
// bus so the same Flutter globe used for SmC can render arcs without
// caring which protocol produced them.
package unbounded

import (
	"context"
	"log/slog"
	"net"
	"sync"

	C "github.com/getlantern/common"

	"github.com/getlantern/broflake/clientcore"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
)

// ConnectionEvent fires every time a consumer (i.e. a censored client
// being routed through this widget proxy) connects or disconnects via
// the broflake mesh. State: +1 on accept, -1 on close. WorkerIdx is
// broflake's internal worker slot identifier — used by the Flutter
// globe to pair connect/disconnect events for the same arc. Addr is
// the remote consumer's IP if broflake exposes it, otherwise empty.
//
// Shape mirrors radiance/peer.ConnectionEvent so consumers (lantern-
// core's listenPeerConnectionEvents in particular) can subscribe with
// a single discriminator and feed both the SmC and Unbounded streams
// into the same globe view.
type ConnectionEvent struct {
	events.Event
	State     int    `json:"state"`
	WorkerIdx int    `json:"workerIdx"`
	Addr      string `json:"addr"`
}

var manager = &unboundedManager{}

type unboundedManager struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	lastCfg *C.UnboundedConfig // most recent server-supplied config
}

// Enabled reports whether the local opt-in is set. Doesn't say whether
// the proxy is currently running (server flag and config can override).
func Enabled() bool {
	return settings.GetBool(settings.UnboundedKey)
}

// SetEnabled flips the local opt-in. When enabling, the proxy starts
// immediately if a server config is already cached; otherwise it
// starts on the next config event. When disabling, the proxy stops.
// Idempotent — calling with the current value is a no-op.
func SetEnabled(enable bool) error {
	if Enabled() == enable {
		return nil
	}
	if err := settings.Set(settings.UnboundedKey, enable); err != nil {
		return err
	}
	slog.Info("Unbounded widget proxy local opt-in changed", "enabled", enable)
	if enable {
		manager.mu.Lock()
		cfg := manager.lastCfg
		manager.mu.Unlock()
		if cfg != nil {
			manager.start(cfg)
		} else {
			slog.Info("Unbounded: enabled locally, will start when server config arrives")
		}
	} else {
		manager.stop()
	}
	return nil
}

// InitSubscription wires the manager into radiance's config event bus.
// Called once at LocalBackend startup; the subscription lives for the
// process lifetime, so repeated calls would leak goroutines — hence
// the package-level guard.
func InitSubscription() {
	initOnce.Do(func() {
		events.Subscribe(func(evt config.NewConfigEvent) {
			if evt.New == nil {
				return
			}
			// config.Config is a type alias for C.ConfigResponse on
			// the current radiance branch — no nested .ConfigResponse
			// field, just dereference and use directly.
			cfg := *evt.New
			manager.mu.Lock()
			manager.lastCfg = cfg.Unbounded
			running := manager.cancel != nil
			manager.mu.Unlock()

			shouldRun := shouldRunUnbounded(cfg)
			switch {
			case shouldRun && !running:
				manager.start(cfg.Unbounded)
			case !shouldRun && running:
				manager.stop()
			}
		})
	})
}

var initOnce sync.Once

// Stop tears down a running widget proxy. Idempotent. Used as a
// LocalBackend shutdown hook so the broflake goroutines don't outlive
// the radiance process during a graceful exit.
func Stop(_ context.Context) error {
	manager.stop()
	return nil
}

func shouldRunUnbounded(cfg C.ConfigResponse) bool {
	if !settings.GetBool(settings.UnboundedKey) {
		return false
	}
	if !cfg.Features[C.UNBOUNDED] {
		return false
	}
	if cfg.Unbounded == nil {
		return false
	}
	return true
}

func (m *unboundedManager) start(ucfg *C.UnboundedConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return // already running
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	go func() {
		slog.Info("Unbounded: starting broflake widget proxy")

		bfOpt := clientcore.NewDefaultBroflakeOptions()
		bfOpt.ClientType = "widget"
		if ucfg != nil {
			if ucfg.CTableSize > 0 {
				bfOpt.CTableSize = ucfg.CTableSize
			}
			if ucfg.PTableSize > 0 {
				bfOpt.PTableSize = ucfg.PTableSize
			}
		}

		// Wire the broflake connection callback into the radiance event
		// bus so the Flutter globe (and any future abuse aggregation)
		// sees consumer connect/disconnect.
		bfOpt.OnConnectionChangeFunc = func(state int, workerIdx int, addr net.IP) {
			addrStr := ""
			if addr != nil {
				addrStr = addr.String()
			}
			slog.Debug("Unbounded: consumer connection change",
				"state", state, "workerIdx", workerIdx, "addr", addrStr)
			events.Emit(ConnectionEvent{
				State:     state,
				WorkerIdx: workerIdx,
				Addr:      addrStr,
			})
		}

		rtcOpt := clientcore.NewDefaultWebRTCOptions()
		if ucfg != nil {
			if ucfg.DiscoverySrv != "" {
				rtcOpt.DiscoverySrv = ucfg.DiscoverySrv
			}
			if ucfg.DiscoveryEndpoint != "" {
				rtcOpt.Endpoint = ucfg.DiscoveryEndpoint
			}
		}

		egOpt := clientcore.NewDefaultEgressOptions()
		if ucfg != nil {
			if ucfg.EgressAddr != "" {
				egOpt.Addr = ucfg.EgressAddr
			}
			if ucfg.EgressEndpoint != "" {
				egOpt.Endpoint = ucfg.EgressEndpoint
			}
		}

		// BroflakeConn is for clients routing traffic *through* the
		// mesh. A widget proxy only donates bandwidth, so the conn
		// is unused — discard it.
		_, ui, err := clientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
		if err != nil {
			slog.Error("Unbounded: failed to create broflake widget", "error", err)
			cancel()
			m.mu.Lock()
			m.cancel = nil
			m.mu.Unlock()
			return
		}

		slog.Info("Unbounded: broflake widget proxy started")
		<-ctx.Done()
		slog.Info("Unbounded: stopping broflake widget proxy")
		ui.Stop()
		m.mu.Lock()
		m.cancel = nil
		m.mu.Unlock()
		slog.Info("Unbounded: broflake widget proxy stopped")
	}()
}

func (m *unboundedManager) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}
