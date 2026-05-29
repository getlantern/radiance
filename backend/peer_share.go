package backend

import (
	"context"
	"fmt"
	"log/slog"
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

// newPeerClient constructs the production peer.Client wired against the
// shared kindling HTTP client and the platform device ID. Pulled out of
// NewLocalBackend so the construction site is a one-liner.
func newPeerClient(platformDeviceID string) (*peer.Client, error) {
	api := peer.NewAPI(kindling.HTTPClient(), common.GetBaseURL(), platformDeviceID)
	client, err := peer.NewClient(peer.Config{API: api})
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
	r.peerToggleMu.Lock()
	defer r.peerToggleMu.Unlock()
	toggleCtx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
	defer cancel()
	if enabled {
		if err := r.peerClient.Start(toggleCtx); err != nil {
			if rbErr := settings.Patch(settings.Settings{settings.PeerShareEnabledKey: false}); rbErr != nil {
				slog.Error("peer share rollback failed after Start error",
					"start_err", err, "rollback_err", rbErr)
			}
			return fmt.Errorf("start peer share: %w", err)
		}
		return nil
	}
	if err := r.peerClient.Stop(toggleCtx); err != nil {
		slog.Warn("peer share stop returned error (toggle still off)", "err", err)
	}
	return nil
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
			slog.Warn("peer share auto-resume failed", "err", err)
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
		slog.Warn("peer share stop on backend close returned error", "err", err)
	}
}

// PeerStatus returns the current peer-share session state for the IPC
// /peer/status endpoint.
func (r *LocalBackend) PeerStatus() peer.Status {
	return r.peerClient.CurrentStatus()
}
