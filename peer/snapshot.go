package peer

import (
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/getlantern/lantern-box/tracker/peerconn"
)

// relayRetention keeps a departed client's byte totals in the snapshot long
// enough for the app's 1s poller to observe its final count.
const relayRetention = 60 * time.Second

// Snapshot is the data direct sharing has relayed, in the shape of
// unbounded.Snapshot so the app can fold both with the same code. Like
// Unbounded's, Epoch is fixed for the process and byte counts are cumulative
// across sessions, so a Stop and Start between two polls loses nothing.
type Snapshot struct {
	Epoch     string      `json:"epoch"`
	Running   bool        `json:"running"`
	Peers     []string    `json:"peers"`
	BytesDown uint64      `json:"bytesDown"`
	BytesUp   uint64      `json:"bytesUp"`
	PeerBytes []PeerBytes `json:"peerBytes"`
}

// PeerBytes is the cumulative traffic relayed for one client IP. An entry
// pruned after relayRetention restarts at zero if the IP returns, which
// consumers treat as a reset.
type PeerBytes struct {
	IP   string `json:"ip"`
	Down uint64 `json:"down"`
	Up   uint64 `json:"up"`
}

// relayTally is one client IP's bytes: closed holds its finished
// connections' totals; its open connections are read live from relay.conns.
type relayTally struct {
	closedDown, closedUp uint64
	open                 int
	endedAt              time.Time
}

// relayStats accumulates relayed bytes from the counters lantern-box
// attaches to each connection. It has its own lock because
// Snapshot is polled every second and must not wait on Start or Stop.
type relayStats struct {
	mu    sync.Mutex
	epoch string
	// conns maps each open connection's counter to its "ip:port" source. The
	// counter, not the source, is the key: samizdat multiplexes streams over
	// one TCP conn, so several open streams can share a source, and the same
	// counter comes back on each stream's close.
	conns                map[*peerconn.Counter]string
	ips                  map[string]*relayTally
	closedDown, closedUp uint64
	// endedAt is when the session stopped, or zero while it runs. Stop drops
	// the close events of connections still open, so those are retired by
	// time instead: relayRetention after endedAt, or at the next begin.
	endedAt time.Time
	// gen identifies the running session and changes at every begin and end.
	// A listener callback that resumes after Stop, or after Stop and Start,
	// carries an old generation, so its event is ignored rather than counted
	// after the session it belongs to has ended.
	gen uint64
	now func() time.Time
}

func (r *relayStats) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// begin starts a session and returns its generation. Totals carry on from
// earlier sessions under the same epoch; connections the previous session
// left open are folded into them, and their clients start their retention.
func (r *relayStats) begin() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.epoch == "" {
		r.epoch = strconv.FormatInt(r.clock().UnixNano(), 10)
	}
	now := r.clock()
	for c, source := range r.conns {
		r.foldLocked(c, source, now)
	}
	r.conns = nil
	r.endedAt = time.Time{}
	r.gen++
	return r.gen
}

// end marks the session stopped and ends its generation, so the listener's
// late events are ignored. Its open connections' counters stay readable for
// relayRetention, long enough for the app's poller to see the bytes relayed
// while the box closed, and are then folded into the totals and dropped.
func (r *relayStats) end() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endedAt = r.clock()
	r.gen++
}

// foldLocked moves a finished connection's final count into the totals and its
// client's tally. Caller must hold r.mu and remove c from r.conns.
func (r *relayStats) foldLocked(c *peerconn.Counter, source string, now time.Time) {
	ip, _, err := net.SplitHostPort(source)
	if err != nil {
		ip = source
	}
	down, up := c.Down(), c.Up()
	r.closedDown += down
	r.closedUp += up
	if t := r.ips[ip]; t != nil {
		t.closedDown += down
		t.closedUp += up
		t.open--
		if t.open <= 0 {
			t.open = 0
			t.endedAt = now
		}
	}
}

// pruneLocked drops clients that left more than relayRetention ago, and once a
// stopped session is past it, folds its still-open connections into the
// totals so they stop being held. Caller must hold r.mu.
func (r *relayStats) pruneLocked(now time.Time) {
	if !r.endedAt.IsZero() && now.Sub(r.endedAt) > relayRetention {
		for c := range r.conns {
			r.closedDown += c.Down()
			r.closedUp += c.Up()
		}
		r.conns = nil
		r.ips = nil
		return
	}
	for ip, t := range r.ips {
		if t.open == 0 && now.Sub(t.endedAt) > relayRetention {
			delete(r.ips, ip)
		}
	}
}

// record follows one connection's accept or close. A close folds the
// connection's final count, so totals keep it after the connection is gone.
func (r *relayStats) record(state int, source string, bytes *peerconn.Counter) {
	r.mu.Lock()
	gen := r.gen
	r.mu.Unlock()
	r.recordFor(gen, state, source, bytes)
}

// recordFor is record for the session with generation gen; an event from any
// other session is ignored.
func (r *relayStats) recordFor(gen uint64, state int, source string, bytes *peerconn.Counter) {
	if bytes == nil {
		return
	}
	ip, _, err := net.SplitHostPort(source)
	if err != nil {
		ip = source
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen != r.gen {
		return
	}
	if state > 0 {
		// Pruned here too, so a session no one polls does not keep every
		// client it has ever seen. Before acquiring, so a returning IP's
		// fresh tally does not inherit its pruned totals.
		r.pruneLocked(r.clock())
	}
	if r.ips == nil {
		r.ips = make(map[string]*relayTally)
	}
	if r.conns == nil {
		r.conns = make(map[*peerconn.Counter]string)
	}
	switch {
	case state > 0:
		r.conns[bytes] = source
		t := r.ips[ip]
		if t == nil {
			t = &relayTally{}
			r.ips[ip] = t
		}
		t.open++
	case state < 0:
		if _, ok := r.conns[bytes]; !ok {
			return
		}
		delete(r.conns, bytes)
		r.foldLocked(bytes, source, r.clock())
	}
}

func (r *relayStats) snapshot(running bool) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.clock())
	s := Snapshot{
		Epoch:     r.epoch,
		Running:   running,
		Peers:     []string{},
		BytesDown: r.closedDown,
		BytesUp:   r.closedUp,
		PeerBytes: []PeerBytes{},
	}
	live := make(map[string]PeerBytes, len(r.ips))
	for c, source := range r.conns {
		down, up := c.Down(), c.Up()
		s.BytesDown += down
		s.BytesUp += up
		if running {
			s.Peers = append(s.Peers, source)
		}
		ip, _, err := net.SplitHostPort(source)
		if err != nil {
			ip = source
		}
		pb := live[ip]
		pb.Down += down
		pb.Up += up
		live[ip] = pb
	}
	for ip, t := range r.ips {
		pb := live[ip]
		s.PeerBytes = append(s.PeerBytes, PeerBytes{IP: ip, Down: t.closedDown + pb.Down, Up: t.closedUp + pb.Up})
	}
	sort.Strings(s.Peers)
	sort.Slice(s.PeerBytes, func(i, j int) bool { return s.PeerBytes[i].IP < s.PeerBytes[j].IP })
	return s
}

// RelaySnapshot reports what the current or most recent direct-sharing
// session has relayed.
func (c *Client) RelaySnapshot() Snapshot {
	return c.relay.snapshot(c.CurrentStatus().Phase == PhaseServing)
}
