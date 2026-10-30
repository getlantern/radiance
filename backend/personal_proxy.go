package backend

import (
	"errors"
	"log/slog"
	"slices"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"

	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

// friendReach is how the server manager and the ticket source can reach one
// friend.
type friendReach struct {
	direct   bool // the friend's direct outbound is a managed server
	consumer bool // a rooms consumer is
	tickets  bool // the ticket source holds an inviter ticket for the friend's pairing
	answered bool // the ticket source has been answered
}

// live reports whether a selection of the friend stands. Before the ticket
// source is first answered a consumer counts, so a selection persisted across
// a restart is not dropped before its tickets can arrive.
func (f friendReach) live() bool {
	return f.direct || (f.consumer && (f.tickets || !f.answered))
}

// selectable reports whether the friend may be newly selected.
func (f friendReach) selectable() bool {
	return f.direct || (f.consumer && f.tickets)
}

func (r *LocalBackend) friendReach(friend string) friendReach {
	var f friendReach
	for _, srv := range r.srvManager.AllServers() {
		if !srv.IsLantern || !lbA.IsHelperTag(srv.Tag) {
			continue
		}
		if srv.Type == lbC.TypeUnbounded {
			f.consumer = true
		} else if srv.Tag == friend {
			f.direct = true
		}
	}
	f.answered = true
	if f.consumer && r.helperTickets != nil {
		var tickets []string
		tickets, f.answered = r.helperTickets.InviterFor(friend)
		f.tickets = len(tickets) > 0
	}
	return f
}

// personalProxyServer stands in for the tunnel's group over friend's
// outbounds. Like the group, it exists while the friend has a member.
func (r *LocalBackend) personalProxyServer(friend string) (*servers.Server, bool) {
	if !r.friendReach(friend).live() {
		return nil, false
	}
	return &servers.Server{Tag: vpn.PersonalProxySelection(friend), Type: lbC.TypeMutableURLTest}, true
}

// PersonalProxyStatus returns the status of every friend this device invited
// that it can reach directly or has a ticket for, ordered by tag.
func (r *LocalBackend) PersonalProxyStatus() []vpn.PersonalProxyStatus {
	if r.srvManager == nil {
		return []vpn.PersonalProxyStatus{}
	}
	var tags []string
	for _, srv := range r.srvManager.AllServers() {
		if srv.IsLantern && lbA.IsHelperTag(srv.Tag) && srv.Type != lbC.TypeUnbounded {
			tags = append(tags, srv.Tag)
		}
	}
	if r.helperTickets != nil {
		tags = append(tags, r.helperTickets.InviterTags()...)
	}
	slices.Sort(tags)
	tags = slices.Compact(tags)
	statuses := make([]vpn.PersonalProxyStatus, 0, len(tags))
	for _, tag := range tags {
		f := r.friendReach(tag)
		status := vpn.PersonalProxyStatus{Tag: tag, Direct: f.direct, Rooms: f.consumer && f.tickets}
		if f.direct && r.vpnClient != nil {
			status.DirectDelayMs = r.vpnClient.PersonalProxyMember(tag).DirectDelay
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// fallbackFor is where a selection of tag goes once the manager no longer
// offers it: a friend's personal proxy falls back to the personal proxies
// group while it exists, and everything else to auto-select.
func (r *LocalBackend) fallbackFor(tag string) string {
	if _, ok := vpn.ParsePersonalProxySelection(tag); ok {
		if _, found := r.personalProxiesServer(); found {
			return vpn.PersonalProxiesTag
		}
	}
	return vpn.AutoSelectTag
}

// fallBackLocked persists and selects tag's fallback (see fallbackFor), and
// tells the app when a friend's personal proxy was the one withdrawn. The
// caller must hold r.selectionMu.
func (r *LocalBackend) fallBackLocked(tag string) {
	fallback := r.fallbackFor(tag)
	// Persist before notifying the VPN client so the choice survives even if
	// the tunnel isn't running to accept the switch.
	r.persistSelection(fallback)
	if err := r.liveTunnel().SelectServer(fallback); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Warn("Failed to switch to the fallback after the selected server was removed", "fallback", fallback, "error", err)
	}
	if friend, ok := vpn.ParsePersonalProxySelection(tag); ok {
		events.Emit(vpn.PersonalProxyFallbackEvent{Tag: friend, Fallback: fallback})
	}
}

// reconcilePersonalProxy follows a change in the inviter tickets: a selected
// friend with no member left falls back, and the tunnel's groups are brought
// in line with the tickets.
func (r *LocalBackend) reconcilePersonalProxy() {
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	r.clearSelectedIfMissingLocked()
	if err := r.reconcileLiveHelpersLocked(); err != nil {
		slog.Warn("Failed to reconcile helpers after the inviter tickets changed", "error", err)
	}
}
