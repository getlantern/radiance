package backend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"

	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

// friendReach is how the server manager and the ticket source can reach one
// friend.
type friendReach struct {
	direct  bool // the friend's direct outbound is a managed server
	reverse bool // the VPN client keeps the friend's reverse tag
	// reverseKnown: r.reverseFriends holds the friend, whose pairing has had a
	// reverse outbound and has not ended.
	reverseKnown bool
	consumer     bool // a rooms consumer is
	tickets      bool // the ticket source holds an inviter ticket for the friend's pairing
	answered     bool // the ticket source has been answered
}

// live reports whether a selection of the friend stands. Before the ticket
// source is first answered a consumer counts, so a selection persisted across
// a restart is not dropped before its tickets can arrive.
func (f friendReach) live() bool { return f.routable() || f.reverseKnown }

// routable reports whether the tunnel's group for the friend has a member:
// the friend's direct or reverse outbound, or a rooms consumer the tunnel
// admits, by the rule of vpn's roomsReachable.
func (f friendReach) routable() bool {
	return f.direct || f.reverse || (f.consumer && (f.tickets || !f.answered))
}

// selectable reports whether the friend may be newly selected.
func (f friendReach) selectable() bool {
	return f.direct || f.reverse || f.reverseKnown || (f.consumer && f.tickets)
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
	f.reverse = slices.Contains(r.reverseOutbounds(), lbA.ReverseHelperTag(friend))
	f.reverseKnown = r.reverseFriends.has(friend)
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

// connectTicketWait bounds how long a connect waits for the first ticket fetch
// before judging a friend reachable only through rooms. iOS gives a starting
// tunnel about 7.5s, and the tunnel itself still has to be built after this,
// so a slower fetch is refused as before rather than risk the extension being
// killed. A variable so tests can shorten it.
var connectTicketWait = 4 * time.Second

// checkConnectSelection refuses a connect that newly chooses a friend who is
// not selectable. The persisted friend is exempt, so a connect can restore it
// before the ticket source is first answered.
//
// A friend reachable only through rooms is selectable once its inviter ticket
// is held, and tickets are fetched only after the backend starts. On macOS and
// iOS the backend starts with the tunnel, a moment before this check, so with
// auto-connect on (no exemption) the check would always find no ticket yet,
// refuse, and stop the backend with the fetch still running. So when rooms are
// the friend's only way and lantern-cloud hasn't answered yet, it waits for
// the first fetch, up to connectTicketWait or until ctx is done.
func (r *LocalBackend) checkConnectSelection(ctx context.Context, tag string) error {
	friend, ok := vpn.ParsePersonalProxySelection(tag)
	if !ok {
		return nil
	}
	var persisted servers.Server
	if err := settings.GetStruct(settings.SelectedServerKey, &persisted); err == nil &&
		!settings.GetBool(settings.AutoConnectKey) && persisted.Tag == tag {
		return nil
	}
	f := r.friendReach(friend)
	if !f.selectable() && f.consumer && !f.answered && r.helperTickets != nil {
		waitCtx, cancel := context.WithTimeout(ctx, connectTicketWait)
		r.helperTickets.WaitFetched(waitCtx)
		cancel()
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("connect cancelled: %w", err)
		}
		f = r.friendReach(friend)
	}
	if !f.selectable() {
		return fmt.Errorf("failed to connect: %w", vpn.ErrPersonalProxyUnavailable)
	}
	return nil
}

// PersonalProxyStatus returns the status of every friend this device invited
// that it can reach directly or by reverse outbound, or has a ticket for,
// ordered by tag.
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
	for _, reverse := range r.reverseOutbounds() {
		if friend, ok := lbA.HelperTagOfReverse(reverse); ok {
			tags = append(tags, friend)
		}
	}
	tags = append(tags, r.reverseFriends.friends()...)
	if r.helperTickets != nil {
		tags = append(tags, r.helperTickets.InviterTags()...)
	}
	slices.Sort(tags)
	tags = slices.Compact(tags)
	statuses := make([]vpn.PersonalProxyStatus, 0, len(tags))
	for _, tag := range tags {
		f := r.friendReach(tag)
		status := vpn.PersonalProxyStatus{Tag: tag, Direct: f.direct, Reverse: f.reverse && r.reverseSessionLive(tag), Rooms: f.consumer && f.tickets}
		if status.Rooms {
			status.RoomsPath = vpn.RoomsPathUnknown
		}
		if (f.direct || status.Rooms) && r.vpnClient != nil {
			m := r.vpnClient.PersonalProxyMember(tag)
			if f.direct {
				status.DirectDelayMs = m.DirectDelay
			}
			if status.Rooms && m.Rooms {
				status.SetRoomsPath(m.RoomsPath)
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

// reverseSessionLive reports whether the running tunnel holds a live session
// on friend's reverse outbound. Selection uses the configured reverse
// outbounds instead, so a friend whose bridge is between sessions stays
// selected.
func (r *LocalBackend) reverseSessionLive(friend string) bool {
	return r.vpnClient != nil && r.vpnClient.ReverseSessionLive(lbA.ReverseHelperTag(friend))
}

func (r *LocalBackend) reverseOutbounds() []string {
	if r.vpnClient == nil {
		return nil
	}
	return r.vpnClient.ReverseOutbounds()
}

// fallbackFor is where a selection of tag goes once the manager no longer
// offers it: a friend's personal proxy falls back to the personal proxies
// group while it exists, and everything else to auto-select.
func (r *LocalBackend) fallbackFor(tag string) string {
	if _, ok := vpn.ParsePersonalProxySelection(tag); ok && r.personalProxiesLive() {
		return vpn.PersonalProxiesTag
	}
	return vpn.AutoSelectTag
}

// fallBackLocked persists and selects tag's fallback (see fallbackFor), and
// tells the app when a friend's personal proxy or a Lantern server was the one
// withdrawn. A withdrawn Lantern server falls back to a server in the same
// location first, so the user's exit country survives (see
// sameLocationServer). The caller must hold r.selectionMu.
func (r *LocalBackend) fallBackLocked(tag string) {
	if friend, ok := vpn.ParsePersonalProxySelection(tag); ok {
		fallback := r.fallbackFor(tag)
		r.switchToFallbackLocked(fallback)
		events.Emit(vpn.PersonalProxyFallbackEvent{Tag: friend, Fallback: fallback})
		return
	}
	// Read before switching, which clears it.
	var selected servers.Server
	withdrawnLantern := settings.GetStruct(settings.SelectedServerKey, &selected) == nil && selected.Tag == tag && selected.IsLantern
	// Off the withdrawn server first, even when a pin follows: the tunnel may
	// not have the pin's outbound yet.
	r.switchToFallbackLocked(vpn.AutoSelectTag)
	if !withdrawnLantern {
		return
	}
	fallback := vpn.AutoSelectTag
	if srv, ok := r.sameLocationServer(selected); ok {
		r.persistServer(srv)
		r.selectPinLocked(srv.Tag)
		fallback = srv.Tag
	}
	slog.Info("Selected Lantern server was withdrawn, falling back", "tag", tag, "fallback", fallback)
	events.Emit(vpn.SelectedServerFallbackEvent{Tag: tag, Fallback: fallback})
}

// switchToFallbackLocked persists and selects fallback. It persists first so
// the choice survives even if the tunnel isn't running to accept the switch.
func (r *LocalBackend) switchToFallbackLocked(fallback string) {
	r.persistSelection(fallback)
	if err := r.liveTunnel().SelectServer(fallback); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Warn("Failed to switch to the fallback after the selected server was removed", "fallback", fallback, "error", err)
	}
}

// selectPinLocked moves a running tunnel onto the persisted pin tag. The
// manual selector ignores a tag it has no outbound for, as one the config
// update in progress is adding, so the tunnel then stays on auto-select and
// updateServers applies the pin (see applyPendingPinLocked).
func (r *LocalBackend) selectPinLocked(tag string) {
	r.pendingPin = ""
	err := r.liveTunnel().SelectServer(tag)
	if errors.Is(err, vpn.ErrTunnelNotConnected) {
		return
	}
	if err == nil {
		if sel, manual := r.liveTunnel().ManualSelection(); manual && sel == tag {
			return
		}
	}
	r.pendingPin = tag
	if err := r.liveTunnel().SelectServer(vpn.AutoSelectTag); err != nil && !errors.Is(err, vpn.ErrTunnelNotConnected) {
		slog.Warn("Failed to return to auto-select until the pin's outbound is added", "pin", tag, "error", err)
	}
}

// applyPendingPinLocked applies a pin selectPinLocked could not, unless the
// persisted selection has changed since.
func (r *LocalBackend) applyPendingPinLocked() {
	pin := r.pendingPin
	if pin == "" {
		return
	}
	r.pendingPin = ""
	var selected servers.Server
	if settings.GetBool(settings.AutoConnectKey) || settings.GetStruct(settings.SelectedServerKey, &selected) != nil || selected.Tag != pin {
		return
	}
	r.selectPinLocked(pin)
	if r.pendingPin != "" {
		slog.Warn("Tunnel still has no outbound for the same-location pin", "pin", pin)
		r.pendingPin = ""
	}
}

// sameLocationServer returns a Lantern server, other than gone, in gone's city,
// else its country, skipping helpers and hard-demoted servers. Most recently
// probed first, so the pick is deterministic.
func (r *LocalBackend) sameLocationServer(gone servers.Server) (*servers.Server, bool) {
	country := gone.Location.CountryCode
	if country == "" {
		country = gone.Location.Country
	}
	if country == "" {
		return nil, false
	}
	var sameCity, sameCountry []*servers.Server
	for _, srv := range r.srvManager.AllServers() {
		if !srv.IsLantern || srv.Tag == gone.Tag || lbA.IsHelperTag(srv.Tag) || isHardDemoted(srv) {
			continue
		}
		c := srv.Location.CountryCode
		if c == "" {
			c = srv.Location.Country
		}
		if !strings.EqualFold(c, country) {
			continue
		}
		if gone.Location.City != "" && strings.EqualFold(srv.Location.City, gone.Location.City) {
			sameCity = append(sameCity, srv)
		} else {
			sameCountry = append(sameCountry, srv)
		}
	}
	for _, candidates := range [][]*servers.Server{sameCity, sameCountry} {
		if len(candidates) == 0 {
			continue
		}
		slices.SortFunc(candidates, func(a, b *servers.Server) int {
			if c := compareSelectionAge(b, a); c != 0 {
				return c
			}
			return strings.Compare(a.Tag, b.Tag)
		})
		return candidates[0], true
	}
	return nil, false
}

// reconcilePersonalProxy brings the selection and the tunnel's groups in line
// after the inviter tickets change.
func (r *LocalBackend) reconcilePersonalProxy() {
	r.selectionMu.Lock()
	defer r.selectionMu.Unlock()
	if r.selectionClosed {
		return
	}
	r.clearSelectedIfMissingLocked()
	if err := r.reconcileLiveHelpersLocked(); err != nil {
		slog.Warn("Failed to reconcile helpers after the inviter tickets changed", "error", err)
	}
	r.restoreSavedSelectionLocked()
}
