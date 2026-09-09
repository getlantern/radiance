package vpn

import (
	"context"
	"log/slog"
	"time"

	"github.com/sagernet/sing-box/adapter"

	"github.com/getlantern/radiance/common"
)

const (
	// recoveryBaseWait and recoveryMaxWait bound the interval between re-poll
	// attempts while the network stays paused.
	recoveryBaseWait = 3 * time.Second
	recoveryMaxWait  = 30 * time.Second
	// recoveryStopTimeout bounds how long stop waits for an in-flight attempt so
	// teardown cannot hang on one.
	recoveryStopTimeout = 5 * time.Second
)

// boxNetwork is the subset of the box's network manager that netRecovery drives.
type boxNetwork interface {
	UpdateInterfaces() error
	NetworkInterfaces() []adapter.NetworkInterface
	ResetNetwork()
}

// netRecovery re-polls interfaces while the pause manager reports the network paused.
type netRecovery struct {
	network       boxNetwork
	networkPaused func() bool // authoritative pause state
	backoff       *common.Backoff
	armCh         chan struct{}
	wakeCh        chan struct{}
	done          chan struct{}
}

func newNetRecovery(network boxNetwork, networkPaused func() bool) *netRecovery {
	return &netRecovery{
		network:       network,
		networkPaused: networkPaused,
		backoff:       common.NewBackoff(recoveryBaseWait, recoveryMaxWait),
		armCh:         make(chan struct{}, 1),
		wakeCh:        make(chan struct{}, 1),
		done:          make(chan struct{}),
	}
}

// pause wakes the loop to start recovering. Safe from the pause callback; never blocks.
func (r *netRecovery) pause() {
	select {
	case r.armCh <- struct{}{}:
	default:
	}
}

// wake interrupts a backoff wait so the loop re-checks the pause state. Safe from
// the pause callback; never blocks.
func (r *netRecovery) wake() {
	select {
	case r.wakeCh <- struct{}{}:
	default:
	}
}

// run drives recovery until ctx is cancelled.
func (r *netRecovery) run(ctx context.Context) {
	defer close(r.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.armCh:
		}
		// Drop a stale wake so the first attempt honors the full base interval,
		// letting a transient pause clear before we act.
		select {
		case <-r.wakeCh:
		default:
		}
		if r.networkPaused() {
			slog.Debug("Network recovery: network paused, re-polling interfaces")
		}
		r.backoff.Reset()
		for r.networkPaused() {
			r.backoff.WaitOn(ctx, r.wakeCh)
			if ctx.Err() != nil {
				return
			}
			if !r.networkPaused() {
				slog.Debug("Network recovery: network unpaused, stopping")
				break
			}
			if r.attempt() {
				slog.Debug("Network recovery: interfaces returned, network reset")
			}
		}
	}
}

// attempt re-polls interfaces and reports whether the list refilled from empty,
// resetting the network once when it did so stale connections rebind.
func (r *netRecovery) attempt() (recovered bool) {
	had := len(r.network.NetworkInterfaces()) > 0
	if err := r.network.UpdateInterfaces(); err != nil {
		slog.Debug("Network recovery: update interfaces failed", "error", err)
	}
	now := len(r.network.NetworkInterfaces()) > 0
	slog.Debug("Network recovery: re-polled interfaces", "had", had, "now", now)
	if !had && now {
		r.network.ResetNetwork()
		return true
	}
	return false
}

// stop waits for an in-flight attempt to finish. The caller must cancel run's
// context first; stop only bounds the wait so teardown cannot hang.
func (r *netRecovery) stop() {
	select {
	case <-r.done:
	case <-time.After(recoveryStopTimeout):
	}
}
