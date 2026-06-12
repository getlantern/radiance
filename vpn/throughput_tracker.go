package vpn

import (
	"context"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"

	"github.com/getlantern/radiance/common"
)

// Throughput reports network throughput in bits per second.
type Throughput struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// defaultThroughputSampleInterval is longer on mobile because each tick iterates every active
// connection to compute its byte delta; at 1s on a phone this adds up under heavy traffic.
var defaultThroughputSampleInterval = func() time.Duration {
	if common.IsMobile() {
		return 3 * time.Second
	}
	return time.Second
}()

type byteTotals struct {
	up   int64
	down int64
}

// closedDelta carries the final byte counts of a connection that closed since the last sample, so
// its bytes still count toward its outbound's rate for the window in which it closed.
type closedDelta struct {
	id       uuid.UUID
	outbound string
	up       int64
	down     int64
}

// throughputTracker reports network throughput, globally and per outbound tag.
// Throughput is sampled at a fixed interval; readers see the most recent
// completed sample.
type throughputTracker struct {
	manager  *connTracker
	interval time.Duration

	mu               sync.RWMutex
	perOutbound      map[string]Throughput
	globalThroughput Throughput

	seen       map[uuid.UUID]byteTotals
	lastGlobal byteTotals
	lastTickAt time.Time

	// pending holds the final byte counts of connections closed since the last sample. The
	// connTracker pushes here on close; sample drains it.
	pendingMu sync.Mutex
	pending   []closedDelta

	// Scratch maps reused across ticks to avoid excessive allocations
	nextSeen        map[uuid.UUID]byteTotals
	nextPerOutbound map[string]Throughput
	deltas          map[string]byteTotals
}

// newThroughputTracker returns a tracker sampling at interval; a non-positive
// interval selects defaultThroughputSampleInterval.
func newThroughputTracker(manager *connTracker, interval time.Duration) *throughputTracker {
	if interval <= 0 {
		interval = defaultThroughputSampleInterval
	}
	return &throughputTracker{
		manager:         manager,
		interval:        interval,
		perOutbound:     make(map[string]Throughput),
		seen:            make(map[uuid.UUID]byteTotals),
		nextSeen:        make(map[uuid.UUID]byteTotals),
		nextPerOutbound: make(map[string]Throughput),
		deltas:          make(map[string]byteTotals),
	}
}

// Run samples the underlying counters until ctx is canceled. It blocks.
func (s *throughputTracker) Run(ctx context.Context) {
	s.lastTickAt = time.Now()
	gUp, gDown := s.manager.Total()
	s.lastGlobal = byteTotals{up: gUp, down: gDown}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sample(now)
		}
	}
}

func (s *throughputTracker) Global() Throughput {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.globalThroughput
}

// Outbound returns the most recent throughput sample for tag, or a zero
// Throughput if no traffic has been observed for that tag.
func (s *throughputTracker) Outbound(tag string) Throughput {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.perOutbound[tag]
}

// PerOutbound returns a snapshot copy of the most recent per-outbound samples.
func (s *throughputTracker) PerOutbound() map[string]Throughput {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Throughput, len(s.perOutbound))
	for k, v := range s.perOutbound {
		out[k] = v
	}
	return out
}

// recordClosed is called by the connTracker when a connection closes, handing off its final byte
// counts so the next sample can attribute them to the connection's outbound.
func (s *throughputTracker) recordClosed(id uuid.UUID, outbound string, up, down int64) {
	s.pendingMu.Lock()
	s.pending = append(s.pending, closedDelta{id: id, outbound: outbound, up: up, down: down})
	s.pendingMu.Unlock()
}

func (s *throughputTracker) applyDelta(id uuid.UUID, outbound string, up, down int64) {
	previous := s.seen[id]

	delta := s.deltas[outbound]
	delta.up += up - previous.up
	delta.down += down - previous.down
	s.deltas[outbound] = delta
}

func (s *throughputTracker) sample(now time.Time) {
	elapsed := now.Sub(s.lastTickAt).Seconds()
	// Skip on clock jumps or coalesced ticks: leaving lastTickAt and the byte baselines
	// untouched means the next sample's elapsed and deltas span the same window. Pending
	// closed records are left for the next real tick to drain.
	if elapsed <= 0 {
		return
	}
	s.lastTickAt = now

	clear(s.deltas)
	clear(s.nextSeen)
	for id, rec := range s.manager.conns.Iter() {
		up := rec.upload.Load()
		down := rec.download.Load()

		s.applyDelta(id, rec.outbound, up, down)
		s.nextSeen[id] = byteTotals{up: up, down: down}
	}

	// Drain pending after the active walk: a connection that closes during the walk is either still
	// seen as active above or captured here, never dropped to the next window.
	s.pendingMu.Lock()
	pending := s.pending
	s.pending = s.pending[:0]
	s.pendingMu.Unlock()
	for _, closed := range pending {
		// A connection still present in the active set this tick was already counted above; skip
		// it so a close racing with the active walk cannot double-count.
		if _, counted := s.nextSeen[closed.id]; counted {
			continue
		}
		s.applyDelta(closed.id, closed.outbound, closed.up, closed.down)
	}
	s.seen, s.nextSeen = s.nextSeen, s.seen

	clear(s.nextPerOutbound)
	for tag, d := range s.deltas {
		s.nextPerOutbound[tag] = Throughput{
			Up:   int64(float64(d.up*8) / elapsed),
			Down: int64(float64(d.down*8) / elapsed),
		}
	}

	gUp, gDown := s.manager.Total()
	globalThroughput := Throughput{
		Up:   int64(float64((gUp-s.lastGlobal.up)*8) / elapsed),
		Down: int64(float64((gDown-s.lastGlobal.down)*8) / elapsed),
	}
	s.lastGlobal = byteTotals{up: gUp, down: gDown}

	s.mu.Lock()
	s.perOutbound, s.nextPerOutbound = s.nextPerOutbound, s.perOutbound
	s.globalThroughput = globalThroughput
	s.mu.Unlock()
}
