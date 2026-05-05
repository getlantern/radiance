package vpn

import (
	"context"
	"sync"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
)

// Throughput reports network throughput in bits per second.
type Throughput struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

const defaultThroughputSampleInterval = time.Second

type byteTotals struct {
	up   int64
	down int64
}

// throughputTracker reports network throughput, globally and per outbound tag.
// Throughput is sampled at a fixed interval; readers see the most recent
// completed sample.
type throughputTracker struct {
	manager  *trafficontrol.Manager
	interval time.Duration

	mu               sync.RWMutex
	perOutbound      map[string]Throughput
	globalThroughput Throughput

	seen       map[uuid.UUID]byteTotals
	lastGlobal byteTotals
	lastTickAt time.Time
}

func newThroughputTracker(manager *trafficontrol.Manager, interval time.Duration) *throughputTracker {
	if interval <= 0 {
		interval = defaultThroughputSampleInterval
	}
	return &throughputTracker{
		manager:     manager,
		interval:    interval,
		perOutbound: make(map[string]Throughput),
		seen:        make(map[uuid.UUID]byteTotals),
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

func (s *throughputTracker) sample(now time.Time) {
	elapsed := now.Sub(s.lastTickAt).Seconds()
	// Skip on clock jumps or coalesced ticks: leaving lastTickAt and the byte baselines
	// untouched means the next sample's elapsed and deltas span the same window.
	if elapsed <= 0 {
		return
	}
	s.lastTickAt = now

	deltas := make(map[string]byteTotals)
	nextSeen := make(map[uuid.UUID]byteTotals, len(s.seen))
	visit := func(m trafficontrol.TrackerMetadata) {
		up := m.Upload.Load()
		down := m.Download.Load()
		prev := s.seen[m.ID]
		d := deltas[m.Outbound]
		d.up += up - prev.up
		d.down += down - prev.down
		deltas[m.Outbound] = d
		nextSeen[m.ID] = byteTotals{up: up, down: down}
	}
	for _, m := range s.manager.Connections() {
		visit(m)
	}
	for _, m := range s.manager.ClosedConnections() {
		visit(m)
	}
	s.seen = nextSeen

	perOutbound := make(map[string]Throughput, len(deltas))
	for tag, d := range deltas {
		perOutbound[tag] = Throughput{
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
	s.perOutbound = perOutbound
	s.globalThroughput = globalThroughput
	s.mu.Unlock()
}
