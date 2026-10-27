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
	RelaySnapshot() peer.Snapshot
	Reregister(ctx context.Context) error
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

// applyPeerShare drives peerClient to match the toggle and is the only writer
// of PeerShareEnabledKey. Persisting and acting happen together under
// peerToggleMu, the same lock the self-stop callback takes, so toggles apply
// in order and the setting always ends up describing the runtime state: "on"
// is rolled back if Start fails. Stop errors are logged because a partial
// teardown shouldn't keep the toggle on.
func (r *LocalBackend) applyPeerShare(enabled bool) error {
	_, err := r.applyPeerShareStarted(enabled)
	return err
}

// applyPeerShareStarted is applyPeerShare, also reporting whether it called
// Start, decided under peerToggleMu. A Start registers with the settings as
// they are, so a caller that persisted a registration setting first knows
// that registration already carries it.
func (r *LocalBackend) applyPeerShareStarted(enabled bool) (started bool, err error) {
	if peerShareUnsupported() {
		// A persisted "on" would otherwise survive every restart with nothing
		// behind it.
		persistErr := persistPeerShare(false)
		if enabled {
			return false, errors.Join(fmt.Errorf("peer share is not supported on %s", common.Platform), persistErr)
		}
		return false, persistErr
	}
	// Construction degrades to a nil client rather than failing (the backend
	// must always come up so a user can report an issue), so the toggle
	// reports the outage instead of panicking.
	if r.peerClient == nil {
		persistErr := persistPeerShare(false)
		if enabled {
			return false, errors.Join(errors.New("peer share unavailable: peer client failed to initialize"), persistErr)
		}
		return false, persistErr
	}
	r.peerToggleMu.Lock()
	defer r.peerToggleMu.Unlock()
	toggleCtx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
	defer cancel()
	if !enabled {
		persistErr := persistPeerShare(false)
		if err := r.peerClient.Stop(toggleCtx); err != nil {
			slog.Warn("peer share stop returned error (toggle still off)", "error", err)
		}
		return false, persistErr
	}
	if err := persistPeerShare(true); err != nil {
		return false, err
	}
	// A second "on" that queued behind an in-flight Start must not fail on
	// "already active" and roll the toggle back under a running peer.
	if r.peerClient.IsActive() {
		return false, nil
	}
	if err := r.peerClient.Start(toggleCtx); err != nil {
		// Surface the underlying Start error so operators can see it in the
		// daemon log (UPnP failure, registration 4xx, etc.) rather than only
		// via the IPC HTTP response.
		slog.Error("peer share start failed", "error", err)
		if rbErr := persistPeerShare(false); rbErr != nil {
			slog.Error("peer share rollback failed after Start error",
				"start_error", err, "rollback_error", rbErr)
		}
		return true, fmt.Errorf("start peer share: %w", err)
	}
	slog.Info("peer share start succeeded")
	return true, nil
}

// reregisterPeer re-registers a running peer in the background, under the
// toggle lock so it cannot overlap a Start or Stop, after "help others too"
// changes. Re-registering rotates the credentials and rebuilds the inbound,
// so every open session, paired ones included, drops and reconnects within
// seconds; the switch's caption warns of it (help_others_too_note). The
// Unbounded widget needs nothing from here: its syncRooms re-reads the
// setting within roomSyncInterval and restarts the widget when helping others
// turns off, with the same brief drop.
func (r *LocalBackend) reregisterPeer() {
	// A closed backend's peer is stopped or stopping; there is nothing to
	// re-register.
	if r.peerClient == nil || r.ctx.Err() != nil || !r.addPeerWork() {
		return
	}
	go func() {
		defer r.peerWG.Done()
		r.peerToggleMu.Lock()
		defer r.peerToggleMu.Unlock()
		if !r.peerClient.IsActive() {
			return
		}
		ctx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
		defer cancel()
		// The message does not name the setting that changed, so this
		// line never ties "help others too" to the peer it describes.
		// ErrNotActive: the peer stopped itself since the check, and the
		// next Start registers with the current value.
		if err := r.peerClient.Reregister(ctx); err != nil && !errors.Is(err, peer.ErrNotActive) {
			slog.Warn("peer re-register failed; the next rotation retries", "error", err)
		}
	}()
}

func persistPeerShare(on bool) error {
	if err := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: on}); err != nil {
		return fmt.Errorf("persist %s: %w", settings.PeerShareEnabledKey, err)
	}
	return nil
}

// onPeerSelfStop clears the persisted toggle when the peer client stopped
// itself, so the setting keeps reflecting runtime state and a restart of the
// app doesn't resume a peer that isn't running. A toggle that restarted the
// client in the meantime wins.
func (r *LocalBackend) onPeerSelfStop(reason error) {
	r.peerToggleMu.Lock()
	defer r.peerToggleMu.Unlock()
	if r.peerClient == nil || r.peerClient.IsActive() {
		return
	}
	slog.Warn("peer share stopped itself; turning the toggle off", "reason", reason)
	if err := persistPeerShare(false); err != nil {
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
	if !r.addPeerWork() {
		return
	}
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

// addPeerWork adds one to peerWG unless the backend is closing, so an Add
// can never race closePeerClient's Wait. It reports whether it added.
func (r *LocalBackend) addPeerWork() bool {
	r.peerWGMu.Lock()
	defer r.peerWGMu.Unlock()
	if r.peerClosing {
		return false
	}
	r.peerWG.Add(1)
	return true
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
	r.peerWGMu.Lock()
	r.peerClosing = true
	r.peerWGMu.Unlock()
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

// PeerRelaySnapshot returns the bytes the current or most recent direct-sharing
// session has relayed, for the IPC /peer/snapshot endpoint. A nil peerClient
// reports an empty snapshot, as PeerStatus does.
func (r *LocalBackend) PeerRelaySnapshot() peer.Snapshot {
	if r.peerClient == nil {
		return peer.Snapshot{Peers: []string{}, PeerBytes: []peer.PeerBytes{}}
	}
	return r.peerClient.RelaySnapshot()
}
