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
	mu     sync.Mutex
	cancel context.CancelFunc
}

func UnboundedEnabled() bool {
	return settings.GetBool(settings.UnboundedKey)
}

func SetUnbounded(enable bool) error {
	if UnboundedEnabled() == enable {
		return nil
	}
	if err := settings.Set(settings.UnboundedKey, enable); err != nil {
		return err
	}
	slog.Info("Updated Unbounded widget proxy", "enabled", enable)
	if enable {
		unbounded.start(nil)
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
		shouldRun := shouldRunUnbounded(cfg)
		unbounded.mu.Lock()
		running := unbounded.cancel != nil
		unbounded.mu.Unlock()

		if shouldRun && !running {
			unbounded.start(cfg.Unbounded)
		} else if !shouldRun && running {
			unbounded.stop()
		}
	})
}

func shouldRunUnbounded(cfg C.ConfigResponse) bool {
	if !settings.GetBool(settings.UnboundedKey) {
		return false
	}
	// When server-side config is available, also check:
	//   cfg.Features[C.UNBOUNDED] && cfg.Unbounded != nil
	// For now, only require the local setting so we can test without
	// lantern-cloud sending the config.
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

		_, ui, err := clientcore.NewBroflake(bfOpt, rtcOpt, egOpt)
		if err != nil {
			slog.Error("Unbounded: failed to create broflake widget", "error", err)
			m.mu.Lock()
			m.cancel = nil
			m.mu.Unlock()
			return
		}

		slog.Info("Unbounded: broflake widget proxy started")
		<-ctx.Done()
		slog.Info("Unbounded: stopping broflake widget proxy")
		ui.Stop()
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

func StopUnbounded(_ context.Context) error {
	unbounded.stop()
	return nil
}
