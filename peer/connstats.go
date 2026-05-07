package peer

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/lantern-box/tracker/peerconn"
)

// connStatsServer is the localhost HTTP endpoint Flutter polls to render
// the live globe. It maintains an in-memory set of active source IPs by
// subscribing to peerconn lifecycle notifications, and serves the current
// snapshot as JSON on GET /peer/connections.
//
// This is a deliberately simple bridge for the prototype: it skips the
// proper Go→FFI→Dart event channel (which Adam's lantern#8492 had a
// pattern for but is on a stale branch with merge conflicts) in favour of
// a poll loop. Replace with a streaming FFI events path once the broader
// peer-share / unbounded plumbing lands; the data shape is intentionally
// the same so Dart consumers don't need to change.
//
// Listen address:
//   - RADIANCE_PEER_STATS_ADDR env var if set (e.g. "127.0.0.1:17099")
//   - default 127.0.0.1:17099
//
// 127.0.0.1 only — never bound to public interfaces. The endpoint reveals
// active proxy clients' IP addresses, which we don't want surfaced to
// anyone outside the local user's machine.
const defaultConnStatsAddr = "127.0.0.1:17099"

type connEntry struct {
	Source   string    `json:"source"`
	Since    time.Time `json:"since"`
	Inbound  int       `json:"-"` // for refcount on duplicate accepts (re-uses)
	id       int       // monotonic id for stable equality across snapshots
}

type connSnapshot struct {
	Sources      []string  `json:"sources"`
	ActiveCount  int       `json:"active_count"`
	GeneratedAt  time.Time `json:"generated_at"`
	ListenerHits int64     `json:"listener_hits"`
}

type connStats struct {
	mu       sync.Mutex
	active   map[string]*connEntry
	hits     int64
	server   *http.Server
	listener net.Listener
}

func newConnStats() *connStats {
	return &connStats{active: make(map[string]*connEntry)}
}

// note records a +1 or -1 transition. Source is "ip:port".
func (s *connStats) note(state int, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits++
	if state == +1 {
		if e, ok := s.active[source]; ok {
			e.Inbound++
			return
		}
		s.active[source] = &connEntry{
			Source:  source,
			Since:   time.Now(),
			Inbound: 1,
		}
	} else if state == -1 {
		if e, ok := s.active[source]; ok {
			e.Inbound--
			if e.Inbound <= 0 {
				delete(s.active, source)
			}
		}
	}
}

func (s *connStats) snapshot() connSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := connSnapshot{
		Sources:      make([]string, 0, len(s.active)),
		ActiveCount:  len(s.active),
		GeneratedAt:  time.Now(),
		ListenerHits: s.hits,
	}
	for src := range s.active {
		out.Sources = append(out.Sources, src)
	}
	return out
}

// start spins up the HTTP server. Returns an error if the listen address
// is already in use; falls back to a kernel-assigned port (":0" suffix)
// only if the configured address conflicts and the env var was unset, so
// users who explicitly pinned a port get a clean failure.
func (s *connStats) start(parent context.Context) error {
	addr := os.Getenv("RADIANCE_PEER_STATS_ADDR")
	envSet := addr != ""
	if !envSet {
		addr = defaultConnStatsAddr
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if envSet {
			return err
		}
		// Default already taken — try a random localhost port so a second
		// app instance still surfaces some endpoint rather than failing.
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
	}
	s.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/peer/connections", func(w http.ResponseWriter, r *http.Request) {
		// Strict localhost gate. net.Listen on 127.0.0.1 already prevents
		// remote connections, but a misconfigured listener (e.g. someone
		// changing addr to ":17099" later) would happily accept LAN
		// requests; this is a defense-in-depth check.
		host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
		if splitErr != nil || !isLoopback(host) {
			http.Error(w, "loopback only", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s.snapshot())
	})

	s.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = s.server.Serve(ln)
	}()

	// Tear down when the parent context is cancelled.
	go func() {
		<-parent.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()
	return nil
}

func (s *connStats) addr() string {
	if s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func isLoopback(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// startConnStats wires the lantern-box peerconn listener through to a new
// connStats instance and starts its HTTP server. Returns the stats object
// (so peer.Client can read snapshots for its own internal stats) and an
// error if the HTTP listener can't be bound.
//
// On success the connection-event listener registered via peerconn is the
// stats notifier; callers SHOULD NOT register a competing listener while
// stats is running. Stop is by cancelling the supplied ctx.
func startConnStats(ctx context.Context) (*connStats, error) {
	s := newConnStats()
	if err := s.start(ctx); err != nil {
		return nil, err
	}
	peerconn.SetListener(func(state int, source string) {
		s.note(state, source)
	})
	go func() {
		<-ctx.Done()
		peerconn.SetListener(nil)
	}()
	return s, nil
}

