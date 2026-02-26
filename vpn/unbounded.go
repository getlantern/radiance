package vpn

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

// UnboundedConnectionEvent is emitted when a consumer connection changes state
// in the broflake widget proxy. State: 1 = connected, -1 = disconnected.
type UnboundedConnectionEvent struct {
	events.Event
	State     int    `json:"state"`
	WorkerIdx int    `json:"workerIdx"`
	Addr      string `json:"addr"`
}

var unbounded = &unboundedManager{}

type unboundedManager struct {
	mu      sync.Mutex
	cancel  context.CancelFunc
	lastCfg *C.UnboundedConfig // most recent config from server
}

// UnboundedEnabled reports whether the Unbounded widget proxy is enabled in local settings.
func UnboundedEnabled() bool {
	return settings.GetBool(settings.UnboundedKey)
}

// SetUnbounded enables or disables the Unbounded widget proxy. When enabling,
// the proxy starts immediately if server config is already available; otherwise
// it will start on the next config event. When disabling, the proxy stops.
func SetUnbounded(enable bool) error {
	if UnboundedEnabled() == enable {
		return nil
	}
	if err := settings.Set(settings.UnboundedKey, enable); err != nil {
		return err
	}
	slog.Info("Updated Unbounded widget proxy", "enabled", enable)
	if enable {
		unbounded.mu.Lock()
		cfg := unbounded.lastCfg
		unbounded.mu.Unlock()
		if cfg != nil {
			unbounded.start(cfg)
		} else {
			slog.Info("Unbounded: enabled locally, will start when server config arrives")
		}
	} else {
		unbounded.stop()
	}
	return nil
}

// InitUnboundedSubscription subscribes to config changes and starts/stops the
// broflake widget proxy based on three conditions:
// 1. settings.UnboundedKey is true (local opt-in)
// 2. cfg.Features["unbounded"] is true (server says run it)
// 3. cfg.Unbounded != nil (server provided discovery/egress URLs)
func InitUnboundedSubscription() {
	events.Subscribe(func(evt config.NewConfigEvent) {
		if evt.New == nil {
			return
		}
		cfg := evt.New.ConfigResponse

		// Always store the latest unbounded config for use by SetUnbounded
		unbounded.mu.Lock()
		unbounded.lastCfg = cfg.Unbounded
		running := unbounded.cancel != nil
		unbounded.mu.Unlock()

		shouldRun := shouldRunUnbounded(cfg)
		if shouldRun && !running {
			// start() is internally guarded against being called when already running.
			unbounded.start(cfg.Unbounded)
		} else if !shouldRun && running {
			// stop() is internally guarded and idempotent; safe to call even
			// if another goroutine changed the running state since we read it.
			unbounded.stop()
		}
	})
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

		// Wire up connection change callback to emit radiance events
		bfOpt.OnConnectionChangeFunc = func(state int, workerIdx int, addr net.IP) {
			addrStr := ""
			if addr != nil {
				addrStr = addr.String()
			}
			slog.Debug("Unbounded: consumer connection change", "state", state, "workerIdx", workerIdx, "addr", addrStr)
			events.Emit(UnboundedConnectionEvent{
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

		// BroflakeConn is for clients routing traffic through the mesh;
		// a widget proxy only donates bandwidth, so the conn is unused.
		_, ui, err := clientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
		if err != nil {
			slog.Error("Unbounded: failed to create broflake widget", "error", err)
			cancel() // cancel the context to avoid a leak
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

// StopUnbounded stops the Unbounded widget proxy. Used as a shutdown hook.
func StopUnbounded(_ context.Context) error {
	unbounded.stop()
	return nil
}
