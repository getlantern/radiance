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

// Snapshot is the data a direct-sharing session has relayed, in the shape of
// unbounded.Snapshot so the app can fold both with the same code. Byte counts
// are cumulative since Epoch, which changes with every session.
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

// relayStats accumulates a session's relayed bytes from the counters
// lantern-box attaches to each connection. It has its own lock because
// Snapshot is polled every second and must not wait on Start or Stop.
type relayStats struct {
	mu                   sync.Mutex
	epoch                string
	conns                map[string]*peerconn.Counter // open, by "ip:port" source
	ips                  map[string]*relayTally
	closedDown, closedUp uint64
	now                  func() time.Time
}

func (r *relayStats) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// begin starts a new session's totals under a fresh epoch. The previous
// session's totals stay readable until then, so a poll just after Stop still
// sees its final bytes.
func (r *relayStats) begin() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.epoch = strconv.FormatInt(r.clock().UnixNano(), 10)
	r.conns = nil
	r.ips = nil
	r.closedDown, r.closedUp = 0, 0
}

// record follows one connection's accept or close. A close folds the
// connection's final count, so totals keep it after the connection is gone.
func (r *relayStats) record(state int, source string, bytes *peerconn.Counter) {
	if bytes == nil {
		return
	}
	ip, _, err := net.SplitHostPort(source)
	if err != nil {
		ip = source
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ips == nil {
		r.ips = make(map[string]*relayTally)
		r.conns = make(map[string]*peerconn.Counter)
	}
	switch {
	case state > 0:
		r.conns[source] = bytes
		t := r.ips[ip]
		if t == nil {
			t = &relayTally{}
			r.ips[ip] = t
		}
		t.open++
	case state < 0:
		if _, ok := r.conns[source]; !ok {
			return
		}
		delete(r.conns, source)
		down, up := bytes.Down(), bytes.Up()
		r.closedDown += down
		r.closedUp += up
		if t := r.ips[ip]; t != nil {
			t.closedDown += down
			t.closedUp += up
			t.open--
			if t.open <= 0 {
				t.open = 0
				t.endedAt = r.clock()
			}
		}
	}
}

func (r *relayStats) snapshot(running bool) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	for ip, t := range r.ips {
		if t.open == 0 && now.Sub(t.endedAt) > relayRetention {
			delete(r.ips, ip)
		}
	}
	s := Snapshot{
		Epoch:     r.epoch,
		Running:   running,
		Peers:     []string{},
		BytesDown: r.closedDown,
		BytesUp:   r.closedUp,
		PeerBytes: []PeerBytes{},
	}
	live := make(map[string]PeerBytes, len(r.ips))
	for source, c := range r.conns {
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
