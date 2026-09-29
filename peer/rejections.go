package peer

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
)

// maxRejectedHosts bounds the tally file. Sharded CDN hostnames are unbounded
// in number, so past the cap new hosts only add to Overflow; the summary groups
// by registrable domain, where the cap is rarely reached.
const maxRejectedHosts = 2000

// maxPortsPerHost bounds each host's port breakdown, which a port scan could
// otherwise grow to every port. Further ports are counted under otherPorts.
const maxPortsPerHost = 16

const otherPorts = "other"

const rejectionSummarySize = 15

// rejectionTally counts destinations this peer refused by a reject rule — the
// server-issued allowlist or an abuse block — so that hosts the allowlist
// wrongly excludes show up without anyone having to read debug logs.
//
// It keeps only the refused host and port. It never records who asked or
// when, since the people using this peer are in censored countries and the
// file sits on a volunteer's disk.
type rejectionTally struct {
	path string

	mu    sync.Mutex
	state rejectionState
	dirty bool
}

type rejectionState struct {
	Hosts map[string]*rejectedHost `json:"hosts"`
	// Overflow counts refusals of new hosts after maxRejectedHosts was reached.
	Overflow int64 `json:"overflow"`
}

type rejectedHost struct {
	Count int64            `json:"count"`
	Ports map[string]int64 `json:"ports"`
}

// rejectedDomain is one line of the summary: refusals grouped by registrable
// domain, or by address for IP literals.
type rejectedDomain struct {
	Domain string
	Count  int64
	Hosts  int
}

// newRejectionTally loads the tally at path. An empty path keeps it in memory
// only. A missing or unreadable file starts a fresh tally rather than failing,
// because the tally is a diagnostic and must never stop the peer from serving.
func newRejectionTally(path string) *rejectionTally {
	t := &rejectionTally{path: path, state: rejectionState{Hosts: map[string]*rejectedHost{}}}
	if path == "" {
		return t
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t
	}
	var loaded rejectionState
	if err == nil {
		err = json.Unmarshal(raw, &loaded)
	}
	if err != nil {
		slog.Warn("peer: starting a fresh rejection tally", "path", path, "err", err)
		return t
	}
	if !validRejectionState(loaded) {
		slog.Warn("peer: starting a fresh rejection tally", "path", path, "err", "invalid saved state")
		return t
	}
	if loaded.Hosts != nil {
		t.state = loaded
	}
	return t
}

// validRejectionState reports whether saved state is safe to resume from.
// Valid JSON can still hold entries record and summary would dereference as
// nil, or more hosts than the cap allows.
func validRejectionState(s rejectionState) bool {
	if len(s.Hosts) > maxRejectedHosts || s.Overflow < 0 {
		return false
	}
	for _, h := range s.Hosts {
		if h == nil || h.Ports == nil || h.Count < 0 {
			return false
		}
		// record only exceeds maxPortsPerHost named ports by adding otherPorts.
		if _, other := h.Ports[otherPorts]; len(h.Ports) > maxPortsPerHost+1 ||
			len(h.Ports) == maxPortsPerHost+1 && !other {
			return false
		}
		for _, n := range h.Ports {
			if n < 0 {
				return false
			}
		}
	}
	return true
}

// record counts one refusal of destination, a "host:port" string. It runs on
// the inbound's connection path, so it only updates memory; flush writes it.
func (t *rejectionTally) record(destination string) {
	host, port, err := net.SplitHostPort(destination)
	if err != nil {
		host, port = destination, ""
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dirty = true
	h, ok := t.state.Hosts[host]
	if !ok {
		if len(t.state.Hosts) >= maxRejectedHosts {
			t.state.Overflow++
			return
		}
		h = &rejectedHost{Ports: map[string]int64{}}
		t.state.Hosts[host] = h
	}
	h.Count++
	if port == "" {
		return
	}
	if _, known := h.Ports[port]; !known && len(h.Ports) >= maxPortsPerHost {
		port = otherPorts
	}
	h.Ports[port]++
}

// summary is ordered most refused first; flush logs its head.
func (t *rejectionTally) summary() (domains []rejectedDomain, total int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	byDomain := map[string]*rejectedDomain{}
	for host, h := range t.state.Hosts {
		key := registrableDomain(host)
		d, ok := byDomain[key]
		if !ok {
			d = &rejectedDomain{Domain: key}
			byDomain[key] = d
		}
		d.Count += h.Count
		d.Hosts++
		total += h.Count
	}
	total += t.state.Overflow
	for _, d := range byDomain {
		domains = append(domains, *d)
	}
	slices.SortFunc(domains, func(a, b rejectedDomain) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Domain, b.Domain))
	})
	return domains, total
}

// flush writes the tally if it changed and logs the top refused domains. The
// write is atomic so a crash mid-write cannot leave a truncated tally that the
// next start would discard.
func (t *rejectionTally) flush() {
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	t.dirty = false
	raw, err := json.MarshalIndent(t.state, "", "  ")
	t.mu.Unlock()
	if err != nil {
		slog.Warn("peer: encoding rejection tally", "err", err)
		return
	}

	domains, total := t.summary()
	top := make([]string, 0, rejectionSummarySize)
	for _, d := range domains[:min(len(domains), rejectionSummarySize)] {
		top = append(top, d.Domain+"="+strconv.FormatInt(d.Count, 10))
	}
	slog.Info("peer: destinations refused by reject rules", "total", total,
		"domains", len(domains), "top", strings.Join(top, " "), "path", t.path)

	if t.path == "" {
		return
	}
	if err := atomicfile.WriteFile(t.path, raw, fileperm.File); err != nil {
		slog.Warn("peer: writing rejection tally", "path", t.path, "err", err)
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
	}
}

// registrableDomain collapses sharded hostnames such as
// rr1---sn-abc.googlevideo.com to googlevideo.com. IP literals and names the
// public suffix list cannot place are returned unchanged.
func registrableDomain(host string) string {
	if net.ParseIP(host) != nil {
		return host
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}
