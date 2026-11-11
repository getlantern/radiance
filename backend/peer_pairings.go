package backend

import (
	"context"
	"log/slog"
	"time"

	"github.com/getlantern/broflake/common/helperticket"

	"github.com/getlantern/radiance/common/settings"
)

// A volunteer serves its helper pairings over its direct TCP route even with
// Share My Connection off: having a pairing is consent to serve that friend.
// The peer client then runs pairings-only, mapping a port and registering
// serve_pool=false, so the route never reaches the general pool. Sharing and
// the pairings-only route are one client on one port mapping. It is
// best-effort: a router that won't map a port, or a route lantern-cloud can't
// verify, leaves the friend on Unbounded rooms, and the attempt is retried with
// backoff and after a network change.

// pairingRouteRetryFirst and pairingRouteRetryMax bound the backoff between
// failed pairings-only starts. pairingRouteRetryFirst is also the least time
// between attempts a network change can force. Vars so tests can shorten them.
var (
	pairingRouteRetryFirst = time.Minute
	pairingRouteRetryMax   = 30 * time.Minute
)

// hasVolunteerPairings reports whether this device is the volunteer in any
// helper pairing.
func (r *LocalBackend) hasVolunteerPairings() bool {
	return len(r.helperPairingKeys.IDs(string(helperticket.RoleVolunteer))) > 0
}

// peerShareActive reports whether the peer client runs or is meant to, for
// Share My Connection or for pairings only. The reverse client reads it to
// leave the manual port to the peer client.
func (r *LocalBackend) peerShareActive() bool {
	return peerShareOn() || r.peerForPairings.Load()
}

// syncPairingRoute reconciles the pairings-only route in the background:
// mapping a port and registering take seconds.
func (r *LocalBackend) syncPairingRoute() {
	if r.peerClient == nil || peerShareUnsupported() || !r.addPeerWork() {
		return
	}
	go func() {
		defer r.peerWG.Done()
		if r.ctx.Err() != nil {
			return
		}
		r.peerToggleMu.Lock()
		defer r.peerToggleMu.Unlock()
		r.reconcilePairingRouteLocked(true)
	}()
}

// reconcilePairingRouteLocked brings the peer client to what sharing being off
// and this device's volunteer pairings ask for: a pairings-only run while it
// has any, nothing otherwise. Every path that leaves sharing off ends here; with
// sharing on, its toggle governs the client and this does nothing. A pool run
// left over from sharing is handed to pairings-only on the same port mapping by
// re-registering, and fails closed: if the re-register fails, the client stops
// rather than leave a pool route up. A fresh start runs here only when startNow
// is set; otherwise it is left to syncPairingRoute, so a toggle returns
// promptly. The caller holds peerToggleMu.
func (r *LocalBackend) reconcilePairingRouteLocked(startNow bool) {
	if r.peerClient == nil || peerShareUnsupported() || settings.GetBool(settings.PeerShareEnabledKey) {
		return
	}
	want, active := r.hasVolunteerPairings(), r.peerClient.IsActive()
	if active && want && !r.peerForPairings.Load() {
		r.peerForPairings.Store(true)
		ctx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
		err := r.peerClient.Reregister(ctx)
		cancel()
		if err == nil {
			r.resetPairingRouteRetry()
			return
		}
		slog.Warn("pairing route re-register failed; restarting it")
		r.stopPeerLocked()
		active = false
	}
	switch {
	case active && !want:
		r.stopPeerLocked()
		r.resetPairingRouteRetry()
	case want && !active && startNow:
		r.startPairingRouteLocked()
	case want && !active:
		r.syncPairingRoute()
	}
}

// startPairingRouteLocked starts a pairings-only run, retrying later if it
// fails. The caller holds peerToggleMu.
func (r *LocalBackend) startPairingRouteLocked() {
	ctx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
	defer cancel()
	// The reverse client may hold the manual port the peer client binds.
	resume := r.pauseReverse(ctx)
	defer resume()
	r.notePairingRouteAttempt()
	r.peerForPairings.Store(true)
	if err := r.peerClient.Start(ctx); err != nil {
		r.peerForPairings.Store(false)
		slog.Warn("pairing route start failed; retrying later", "error", err)
		r.schedulePairingRouteRetry()
		return
	}
	r.resetPairingRouteRetry()
	slog.Info("pairing route started")
}

// stopPeerLocked stops the peer client. The caller holds peerToggleMu.
func (r *LocalBackend) stopPeerLocked() {
	ctx, cancel := context.WithTimeout(r.ctx, peerToggleTimeout)
	defer cancel()
	if err := r.peerClient.Stop(ctx); err != nil {
		slog.Warn("peer stop returned error", "error", err)
	}
	r.peerForPairings.Store(false)
	r.restartYieldedReverse()
}

// schedulePairingRouteRetry tries the pairings-only start again after a
// backoff that doubles with each failure up to pairingRouteRetryMax. Nothing is
// scheduled once the backend is closing.
func (r *LocalBackend) schedulePairingRouteRetry() {
	r.peerWGMu.Lock()
	closing := r.peerClosing
	r.peerWGMu.Unlock()
	if closing {
		return
	}
	r.pairingRetryMu.Lock()
	defer r.pairingRetryMu.Unlock()
	if r.pairingRetryTimer != nil {
		r.pairingRetryTimer.Stop()
	}
	wait := r.pairingRetryWait
	if wait == 0 {
		wait = pairingRouteRetryFirst
	}
	r.pairingRetryWait = min(2*wait, pairingRouteRetryMax)
	r.pairingRetryTimer = time.AfterFunc(wait, r.syncPairingRoute)
}

// resetPairingRouteRetry cancels a pending retry and restarts the backoff.
func (r *LocalBackend) resetPairingRouteRetry() {
	r.pairingRetryMu.Lock()
	defer r.pairingRetryMu.Unlock()
	if r.pairingRetryTimer != nil {
		r.pairingRetryTimer.Stop()
		r.pairingRetryTimer = nil
	}
	r.pairingRetryWait = 0
}

// notePairingRouteAttempt records when a pairings-only start was last tried.
func (r *LocalBackend) notePairingRouteAttempt() {
	r.pairingRetryMu.Lock()
	r.pairingLastAttempt = time.Now()
	r.pairingRetryMu.Unlock()
}

// pairingRouteNetworkChanged retries a failed pairings-only start at once, as
// the new network's router may map a port the last one wouldn't, but no more
// often than pairingRouteRetryFirst: network events can come in bursts.
func (r *LocalBackend) pairingRouteNetworkChanged() {
	if r.peerClient == nil || r.peerClient.IsActive() || !r.hasVolunteerPairings() {
		return
	}
	r.pairingRetryMu.Lock()
	recent := time.Since(r.pairingLastAttempt) < pairingRouteRetryFirst
	r.pairingRetryMu.Unlock()
	if recent {
		return
	}
	r.resetPairingRouteRetry()
	r.syncPairingRoute()
}
