package peer

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/radiance/events"
)

// AbuseSummaryEvent fires periodically (every flush interval) carrying a
// snapshot of recent peer-share connection activity bucketed by source
// IP and destination port-class. It's the input that downstream consumers
// (lantern-cloud abuse aggregator, Flutter "your peer is being used for
// X" diagnostic, future bandit blocklist) reason over.
//
// Why per-source AND per-port-class:
//
//   - source distinguishes one client from another for client-level
//     blocklist decisions (one client doing 99% of the spam attempts
//     should get cut off, the other clients should not).
//   - port class lets the consumer reason about TYPE of activity
//     without per-destination granularity, which would be both very
//     noisy (long tail of HTTPS destinations) and privacy-sensitive
//     (the destination list is the user's browsing history of the
//     people using their connection).
//
// We deliberately do NOT carry full destination hostnames in the
// summary — only the port-class bucket and a count. The destination
// tail is the highest privacy-cost piece and the lowest abuse-
// detection value piece.
type AbuseSummaryEvent struct {
	events.Event
	// Window the summary covers, [Start, End].
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// Sources is keyed by the connecting client's "ip:port" with the
	// port stripped (so multiple connections from the same client IP
	// roll up). Counts are connections accepted in the window (not
	// "currently active").
	Sources []SourceBucket `json:"sources"`
}

// SourceBucket aggregates a single source IP's activity within a
// summary window. PortClassCounts maps a port-class label
// (smtp/irc/https/http/dns/other) to the number of accepted
// connections to destinations in that class.
type SourceBucket struct {
	SourceIP        string         `json:"source_ip"`
	TotalAccepts    int            `json:"total_accepts"`
	PortClassCounts map[string]int `json:"port_class_counts"`
}

// abuseAggregator buckets per-(source, port-class) within a rolling
// window. Single-active per peer.Client; the lifecycle is bound to the
// client's runCtx (Start/Stop create + tear it down).
type abuseAggregator struct {
	mu            sync.Mutex
	windowStart   time.Time
	bySource      map[string]*srcStats // keyed by source IP (no port)
	flushInterval time.Duration
}

type srcStats struct {
	totalAccepts    int
	portClassCounts map[string]int
}

func newAbuseAggregator(flushInterval time.Duration) *abuseAggregator {
	if flushInterval <= 0 {
		flushInterval = 5 * time.Minute
	}
	return &abuseAggregator{
		windowStart:   time.Now(),
		bySource:      make(map[string]*srcStats),
		flushInterval: flushInterval,
	}
}

// note records one accept event. Cheap (one map lookup, one increment);
// safe to call from the lantern-box peerconn listener path which fires
// synchronously on the inbound's accept loop.
func (a *abuseAggregator) note(source, destination string) {
	if source == "" {
		return
	}
	srcIP := stripPort(source)
	if srcIP == "" {
		return
	}
	class := classifyPort(destination)

	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.bySource[srcIP]
	if !ok {
		s = &srcStats{portClassCounts: make(map[string]int)}
		a.bySource[srcIP] = s
	}
	s.totalAccepts++
	s.portClassCounts[class]++
}

// flush returns the current window's contents and rolls the window
// forward. Returns nil if no activity was observed in the window
// (avoid emitting empty summaries that just clutter the event stream).
func (a *abuseAggregator) flush() *AbuseSummaryEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bySource) == 0 {
		// Roll the window even on empty so the start timestamp doesn't
		// drift backwards relative to clock progress.
		a.windowStart = time.Now()
		return nil
	}
	now := time.Now()
	evt := &AbuseSummaryEvent{
		WindowStart: a.windowStart,
		WindowEnd:   now,
		Sources:     make([]SourceBucket, 0, len(a.bySource)),
	}
	for srcIP, stats := range a.bySource {
		evt.Sources = append(evt.Sources, SourceBucket{
			SourceIP:        srcIP,
			TotalAccepts:    stats.totalAccepts,
			PortClassCounts: stats.portClassCounts,
		})
	}
	// Stable order so the same window content always serializes the
	// same way — easier to diff in tests, easier to read in logs.
	sort.Slice(evt.Sources, func(i, j int) bool {
		return evt.Sources[i].SourceIP < evt.Sources[j].SourceIP
	})
	a.bySource = make(map[string]*srcStats)
	a.windowStart = now
	return evt
}

// runFlushLoop periodically drains the window and emits an
// AbuseSummaryEvent on the radiance event bus. Returns when ctx is
// cancelled (called from peer.Client.Start, ctx is the same runCtx
// that gates the heartbeat / cred-rotation goroutines).
func (a *abuseAggregator) runFlushLoop(ctx context.Context) {
	t := time.NewTicker(a.flushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// One final flush so the last window's data isn't lost on
			// graceful shutdown — useful when Stop runs right after a
			// burst of connections during a peer rotation, etc.
			if evt := a.flush(); evt != nil {
				events.Emit(*evt)
			}
			return
		case <-t.C:
			if evt := a.flush(); evt != nil {
				events.Emit(*evt)
			}
		}
	}
}

// stripPort returns the host portion of an "ip:port" string, or the
// input unchanged if it doesn't parse cleanly. Supports IPv6 bracketed
// hosts ("[::1]:443" → "::1"). Empty in/out means "no signal".
func stripPort(addr string) string {
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr // best-effort: surface what we got
	}
	return host
}

// classifyPort maps a "host:port" destination to a coarse abuse-relevant
// port-class label. Designed to be tiny on purpose — finer buckets
// would leak the destination tail, which is what we're trying NOT to
// log. The classes:
//
//   smtp  — SMTP / submission / SMTPS (25, 465, 587, 2525). Spam relays.
//   irc   — IRC / IRCS (6660-6669, 6697). Botnet C2.
//   https — 443 / 8443. Bulk legitimate traffic, also credential stuffing.
//   http  — 80 / 8080 / 8000. Bulk legitimate, also some scraping.
//   dns   — 53 / 853. Mostly legit; flagged because a client doing many
//           DNS resolutions through a peer is unusual and could indicate
//           DNS tunneling.
//   other — everything else. Volume here is normally near zero on a
//           samizdat peer; spikes suggest something protocol-specific.
func classifyPort(destination string) string {
	if destination == "" {
		return "other"
	}
	_, portStr, err := net.SplitHostPort(destination)
	if err != nil {
		// Sometimes "destination" is a hostname without a port; rare
		// but possible if upstream callers pass a partial value.
		// Treat as "other" rather than panicking.
		return "other"
	}
	switch portStr {
	case "25", "465", "587", "2525":
		return "smtp"
	case "443", "8443":
		return "https"
	case "80", "8080", "8000":
		return "http"
	case "53", "853":
		return "dns"
	case "6660", "6661", "6662", "6663", "6664", "6665",
		"6666", "6667", "6668", "6669", "6697":
		return "irc"
	}
	// Range checks for IRC (some servers use other ports in the range)
	// and a quick range read for "other" — keep cheap, no regex.
	if strings.HasPrefix(portStr, "66") && len(portStr) == 4 {
		return "irc"
	}
	return "other"
}
