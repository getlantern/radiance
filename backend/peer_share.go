package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/peer"
)

// peerController is the subset of *peer.Client that LocalBackend needs.
// Defined as an interface so tests can swap in a fake without standing up
// real UPnP / sing-box / lantern-cloud dependencies.
type peerController interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	IsActive() bool
	CurrentStatus() peer.Status
}

// peerToggleTimeout caps blocking time on a slow router; UPnP M-SEARCH +
// /v1/peer/register normally complete in single-digit seconds. Also used as
// the deadline for Stop on backend close so a stalled deregister can't hang
// shutdown.
const peerToggleTimeout = 30 * time.Second

// peerShareUnsupported reports platforms that must never serve as a peer. It
// is a var solely so tests can reach the unsupported branch: common.Platform
// is a constant, so the real check cannot be faked at runtime.
//
// iOS is excluded because the backend runs inside the network extension
// there, on a memory budget roughly a tenth of every other platform's, and
// serving adds a second sing-box instance to it. The memory monitor cannot
// absorb the difference: it measures the whole process but can only reclaim
// the VPN's own connections, so peer load would be relieved by evicting the
// user's own traffic, and failing that the extension is killed and the VPN
// goes down with it.
var peerShareUnsupported = common.IsIOS

// newPeerClient constructs the production peer.Client wired against the
// shared kindling HTTP client and the platform device ID. Pulled out of
// NewLocalBackend so the construction site is a one-liner.
func newPeerClient(platformDeviceID string, onSelfStop func(error)) (*peer.Client, error) {
	api := peer.NewAPI(kindling.HTTPClient(), common.GetBaseURL(), platformDeviceID)
	client, err := peer.NewClient(peer.Config{
		API:            api,
		RejectionsPath: filepath.Join(settings.GetString(settings.DataPathKey), "peer-rejections.json"),
		OnSelfStop:     onSelfStop,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create peer client: %w", err)
	}
	return client, nil
}

// applyPeerShare drives peerClient to match the toggle. On Start failure the
// persisted setting is rolled back so reads of PeerShareEnabledKey reflect
// runtime state. Stop errors are logged because a partial teardown shouldn't
// keep the toggle on.
//
// peerToggleMu serializes concurrent toggles: without it, a fast off→on→off
// sequence could see the second call's "already active" rollback racing the
// third call's Stop.
func (r *LocalBackend) applyPeerShare(enabled bool) error {
	if peerShareUnsupported() {
		if enabled {
			// Same reason as the nil-client path below: a persisted "on"
			// would otherwise survive every restart with nothing behind it.
			if rbErr := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: false}); rbErr != nil {
				slog.Error("peer share rollback failed on unsupported platform", "error", rbErr)
			}
			return fmt.Errorf("peer share is not supported on %s", common.Platform)
		}
		return nil
	}
	// Construction degrades to a nil client rather than failing (the backend
	// must always come up so a user can report an issue), so the toggle
	// reports the outage instead of panicking. Roll the setting back, or a
	// persisted "on" would survive with nothing behind it.
	if r.peerClient == nil {
		if enabled {
			if rbErr := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: false}); rbErr != nil {
				slog.Error("peer share rollback failed with no peer client", "error", rbErr)
			}
			return errors.New("peer share unavailable: peer client failed to initialize")
		}
		return nil
	}
	r.peerToggleMu.Lock()
	defer r.peerToggleMu.Unlock()
	toggleCtx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
	defer cancel()
	if enabled {
		// A second "on" that queued behind an in-flight Start must not fail on
		// "already active", or the rollback below would clear the toggle under
		// a running peer.
		if r.peerClient.IsActive() {
			return nil
		}
		if err := r.peerClient.Start(toggleCtx); err != nil {
			// Surface the underlying Start error so operators can see it
			// in the daemon log (UPnP failure, registration 4xx, etc.)
			// rather than only via the IPC HTTP response.
			slog.Error("peer share start failed", "error", err)
			if rbErr := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: false}); rbErr != nil {
				slog.Error("peer share rollback failed after Start error",
					"start_error", err, "rollback_error", rbErr)
			}
			return fmt.Errorf("start peer share: %w", err)
		}
		slog.Info("peer share start succeeded")
		// A self-stop callback may have cleared the toggle between the caller
		// deciding to start and this Start; it runs under peerToggleMu too, so
		// the running peer's state is re-asserted here.
		if !settings.GetBool(settings.PeerShareEnabledKey) {
			if err := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: true}); err != nil {
				slog.Error("re-asserting peer share toggle after start", "error", err)
			}
		}
		return nil
	}
	if err := r.peerClient.Stop(toggleCtx); err != nil {
		slog.Warn("peer share stop returned error (toggle still off)", "error", err)
	}
	return nil
}

// onPeerSelfStop clears the persisted toggle when the peer client stopped
// itself, so the setting keeps reflecting runtime state. Left on, it would make
// the next "on" a no-op, since PatchSettings only acts on changed keys. A
// toggle that restarted the client in the meantime wins.
func (r *LocalBackend) onPeerSelfStop(reason error) {
	r.peerToggleMu.Lock()
	defer r.peerToggleMu.Unlock()
	if r.peerClient == nil || r.peerClient.IsActive() {
		return
	}
	slog.Warn("peer share stopped itself; turning the toggle off", "reason", reason)
	if err := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: false}); err != nil {
		slog.Error("clearing peer share toggle after self-stop", "error", err)
	}
}

// resumePeerShareIfEnabled re-Starts the peer client if the user left the
// toggle on across restarts. Runs in a goroutine because UPnP discovery and
// registration can take several seconds and Start() must return promptly.
// peerWG ensures Close waits for an in-flight resume to settle before
// teardown, so we never leave a registered route or a running box behind.
func (r *LocalBackend) resumePeerShareIfEnabled() {
	if !settings.GetBool(settings.PeerShareEnabledKey) {
		return
	}
	r.peerWG.Add(1)
	go func() {
		defer r.peerWG.Done()
		if r.ctx.Err() != nil {
			return
		}
		if err := r.applyPeerShare(true); err != nil {
			slog.Warn("peer share auto-resume failed", "error", err)
		}
	}()
}

// closePeerClient runs at backend shutdown. It waits for any in-flight
// auto-resume Start to finish (so we don't tear down ctx while it's still
// setting things up — that would leave a registered route + open box
// behind) and then stops the peer client with a fresh ctx so Deregister
// and UnmapPort have a live HTTP deadline even though r.ctx is about to
// cancel.
//
// No-op when peerClient is nil (peer-focused unit tests that construct
// partial LocalBackends).
func (r *LocalBackend) closePeerClient() {
	if r.peerClient == nil {
		return
	}
	r.peerWG.Wait()
	if !r.peerClient.IsActive() {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), peerToggleTimeout)
	defer cancel()
	if err := r.peerClient.Stop(stopCtx); err != nil {
		slog.Warn("peer share stop on backend close returned error", "error", err)
	}
}

// PeerStatus returns the current peer-share session state for the IPC
// /peer/status endpoint. A nil peerClient reports the zero Status rather
// than panicking: construction degrades to a nil client, and the IPC
// handler serves this on every GET /peer/status.
func (r *LocalBackend) PeerStatus() peer.Status {
	if r.peerClient == nil {
		return peer.Status{}
	}
	return r.peerClient.CurrentStatus()
}

// restartsStoppedPeer reports whether a settings patch asks for peer share
// while it is persisted on but not running, which the diff alone can't see.
func (r *LocalBackend) restartsStoppedPeer(updates, diff settings.Settings) bool {
	on, ok := updates[settings.PeerShareEnabledKey].(bool)
	if _, changed := diff[settings.PeerShareEnabledKey]; !ok || !on || changed || r.peerClient == nil {
		return false
	}
	return !r.peerClient.IsActive()
}
