package unbounded

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/getlantern/broflake/clientcore"
	box "github.com/getlantern/lantern-box"
	lblog "github.com/getlantern/lantern-box/log"
	"github.com/getlantern/lantern-box/node"
	"github.com/getlantern/lantern-box/protocol/unbounded/exit"

	rcommon "github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal/hostaccess"
	rlog "github.com/getlantern/radiance/log"
)

// directExitFeature is config-new's feature for a volunteer that exits users' traffic itself
// rather than relaying it to the egress server.
const directExitFeature = "direct_exit"

// directExitOffAfter is how many configs in a row must leave direct_exit off before a running
// exit stops. lantern-cloud can drop the flag from one config when a lookup behind it fails, and
// stopping ends every session the exit serves.
const directExitOffAfter = 2

// autoRedirectOutputMark is sing-tun's DefaultAutoRedirectOutputMark, which radiance's Linux tun
// keeps (it sets auto_redirect with no output mark). The exit box's sockets carry it, so the
// tun's nftables rules don't redirect exit traffic into the volunteer's own tunnel.
const autoRedirectOutputMark = 0x2024

// directExitUnsupported: on iOS the backend runs in the network extension, which can't hold the
// exit's second sing-box, as for peer sharing (backend/peer_share.go).
var directExitUnsupported = rcommon.IsIOS

// directExit is the part of exit.Exit the manager drives.
type directExit interface {
	Options() *clientcore.ExitOptions
	SetEnabled(on bool)
	Close()
}

// newDirectExit makes the widget's exit. It fails without with_quic.
var newDirectExit = func(ctx context.Context) (directExit, error) {
	e, err := exit.New(ctx, lblog.NewFactory(rlog.RedactConnectionInfo(slog.Default().Handler())).NewLogger("unbounded-exit"))
	if err != nil {
		return nil, err
	}
	return e, nil
}

// runDirectExit is node.RunExit for the exit newDirectExit made.
var runDirectExit = func(ctx context.Context, e directExit, cfg node.ExitConfig) error {
	cfg.Exit = e.(*exit.Exit)
	return node.RunExit(ctx, cfg)
}

// exitFlag is config-new's direct_exit, which turns on at once and off only after
// directExitOffAfter configs in a row without it.
type exitFlag struct {
	on     bool
	misses int
}

func (f *exitFlag) update(on bool) bool {
	if on {
		f.on, f.misses = true, 0
		return true
	}
	f.misses++
	if f.misses >= directExitOffAfter {
		f.on = false
	}
	return f.on
}

// exitHeaders gives RunExit's registrations the headers config-new's requests carry, as the peer
// API's do. That replaces lantern-box's X-Lantern-App, which lantern-cloud refuses in webrtc
// mode, and sends the user ID config-new gets, so the direct_exit flag's percentage agrees.
// RunExit's device ID and content type stay.
func exitHeaders(h http.Header) {
	r, err := rcommon.NewRequestWithHeaders(context.Background(), http.MethodPost, "http://exit.invalid", nil)
	if err != nil {
		return
	}
	for k, v := range r.Header {
		if k == http.CanonicalHeaderKey(rcommon.DeviceIDHeader) {
			continue
		}
		h[k] = v
	}
	if val := settings.GetString(settings.FeatureOverridesKey); val != "" {
		h.Set("X-Lantern-Feature-Override", val)
	}
}

// SetDirectExitAllowed sets the check for whether this device may exit users' traffic at all:
// its country is known and not censored. nil, the default, never allows it.
func SetDirectExitAllowed(allowed func() bool) {
	manager.mu.Lock()
	manager.exitAllowed = allowed
	run := manager.exitRun
	manager.mu.Unlock()
	if run != nil {
		run.sync()
	}
}

// exitWantedLocked reports whether the exit should be on. Caller must hold m.mu.
func (m *unboundedManager) exitWantedLocked() bool {
	return m.exitFlag.on && m.exitAllowed != nil && m.exitAllowed()
}

func (m *unboundedManager) exitWanted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exitWantedLocked()
}

func (m *unboundedManager) setExitHoldOff(until time.Time) {
	m.mu.Lock()
	m.exitNotBefore = until
	m.mu.Unlock()
}

func (m *unboundedManager) exitHoldOff() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exitNotBefore
}

// reregisterExit asks the running exit, if any, to register again, as the serve-pool setting
// changed.
func (m *unboundedManager) reregisterExit() {
	m.mu.Lock()
	run := m.exitRun
	m.mu.Unlock()
	if run != nil {
		run.reregisterNow()
	}
}

// startExit makes a widget run's exit, or returns nil where there is none: on iOS, or where
// the exit can't be made. It starts off.
func (m *unboundedManager) startExit() *exitRun {
	if directExitUnsupported() {
		return nil
	}
	e, err := newDirectExit(box.BaseContext())
	if err != nil {
		slog.Warn("Unbounded: no direct exit", "error", err)
		return nil
	}
	return newExitRun(e, node.ExitConfig{
		APIURL:      rcommon.GetBaseURL(),
		DeviceID:    settings.GetString(settings.DeviceIDKey),
		HTTPClient:  bypassHTTPClient,
		Header:      exitHeaders,
		Version:     rcommon.GetVersion(),
		NewBox:      newExitBox,
		OnHoldOff:   m.setExitHoldOff,
		RoutingMark: autoRedirectOutputMark,
		ServePool:   servePool,
	}, m.exitWanted, m.exitHoldOff)
}

// exitRun is one widget run's direct exit: the exit broflake offers sessions through, and
// RunExit keeping its registration and boxes current while the exit is on.
type exitRun struct {
	e          directExit
	cfg        node.ExitConfig
	wanted     func() bool
	notBefore  func() time.Time
	reregister chan struct{}

	mu     sync.Mutex
	closed bool
	cancel context.CancelFunc
	done   chan struct{}
}

func newExitRun(e directExit, cfg node.ExitConfig, wanted func() bool, notBefore func() time.Time) *exitRun {
	r := &exitRun{e: e, cfg: cfg, wanted: wanted, notBefore: notBefore, reregister: make(chan struct{}, 1)}
	r.cfg.Reregister = r.reregister
	return r
}

// sync turns the exit on or off to match wanted: on, it enables the exit and starts RunExit;
// off, it disables the exit, ending its sessions, and waits for RunExit to close its boxes.
func (r *exitRun) sync() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if !r.wanted() {
		r.e.SetEnabled(false)
		r.stopLocked()
		return
	}
	r.e.SetEnabled(true)
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(hostaccess.Context(box.BaseContext()))
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	cfg := r.cfg
	cfg.NotBefore = r.notBefore()
	go func() {
		defer close(done)
		// RunExit logs and retries lantern-cloud's and the boxes' failures itself. It returns an
		// error only for a config it can't run with, which names no address or ID.
		if err := runDirectExit(ctx, r.e, cfg); err != nil && ctx.Err() == nil {
			slog.Error("Unbounded: direct exit can't run", "error", err)
		}
	}()
}

func (r *exitRun) stopLocked() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	<-r.done
	r.cancel, r.done = nil, nil
}

// reregisterNow asks RunExit to register again at once, without blocking.
func (r *exitRun) reregisterNow() {
	select {
	case r.reregister <- struct{}{}:
	default:
	}
}

// close stops RunExit and closes the exit for good. Call it once the widget has stopped, so
// broflake hands the exit no more paths.
func (r *exitRun) close() {
	r.mu.Lock()
	r.closed = true
	r.e.SetEnabled(false)
	r.stopLocked()
	r.mu.Unlock()
	r.e.Close()
}
