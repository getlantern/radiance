package unbounded

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/broflake/clientcore"
)

var snapshotEpoch = strconv.FormatInt(time.Now().UnixNano(), 10)

// peerRetention keeps a departed consumer's byte totals in the snapshot long
// enough for the app's 1s poller to observe its final count.
const peerRetention = 60 * time.Second

// Snapshot describes the current widget state, including peers missed by event subscribers.
// Byte counts are cumulative since Epoch, like Arrivals.
type Snapshot struct {
	Epoch     string      `json:"epoch"`
	Arrivals  uint64      `json:"arrivals"`
	Enabled   bool        `json:"enabled"`
	Running   bool        `json:"running"`
	Peers     []string    `json:"peers"`
	BytesDown uint64      `json:"bytesDown"`
	BytesUp   uint64      `json:"bytesUp"`
	PeerBytes []PeerBytes `json:"peerBytes"`
}

// PeerBytes is the cumulative traffic relayed for one consumer IP. Counts only
// grow while the entry exists; an entry pruned after peerRetention restarts at
// zero if the IP returns, which consumers treat as a reset.
type PeerBytes struct {
	IP   string `json:"ip"`
	Down uint64 `json:"down"`
	Up   uint64 `json:"up"`
}

// slotBytes holds bytes relayed through one broflake worker slot since they
// were last folded into the manager's totals. Chunk callbacks only touch these
// atomics, so the per-chunk data path never contends on m.mu.
type slotBytes struct {
	down, up atomic.Uint64
}

// slotTable is created per worker run so chunks a stopped worker delivers late
// land in an orphaned table instead of the next run's attribution.
type slotTable struct {
	slots sync.Map
	// mu is read-held by each chunk callback around its add, so close can
	// wait out callbacks already past the closed check before the final fold.
	mu     sync.RWMutex
	closed bool
}

func (t *slotTable) add(idx int, size uint64, down bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.closed {
		return
	}
	s := t.slot(idx)
	if down {
		s.down.Add(size)
	} else {
		s.up.Add(size)
	}
}

// close stops the table accepting bytes and returns once every in-flight add
// has landed, so a fold after close sees all accepted bytes.
func (t *slotTable) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
}

func (t *slotTable) slot(idx int) *slotBytes {
	if s, ok := t.slots.Load(idx); ok {
		return s.(*slotBytes)
	}
	s, _ := t.slots.LoadOrStore(idx, &slotBytes{})
	return s.(*slotBytes)
}

type peerTally struct {
	down, up uint64
	// active counts slots currently held by this IP; endedAt is set when it drops to zero.
	active  int
	endedAt time.Time
}

// chunkCounter returns a broflake ChunkFunc that adds relayed bytes to the
// slot's downstream or upstream counter. It keeps counting after the worker's
// context is cancelled: broflake tears down asynchronously after ui.Stop
// returns and can still deliver bytes, which the retired run absorbs.
func chunkCounter(slots *slotTable, down bool) clientcore.ChunkFunc {
	return func(size int, workerIdx int) {
		if size <= 0 {
			return
		}
		slots.add(workerIdx, uint64(size), down)
	}
}

// CurrentSnapshot returns an independent copy of the live widget state.
func CurrentSnapshot() Snapshot {
	return manager.snapshot()
}

func (m *unboundedManager) snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prunePeerBytes()
	s := Snapshot{
		Epoch:     snapshotEpoch,
		Arrivals:  m.arrivals,
		Enabled:   Enabled(),
		Running:   m.running,
		Peers:     []string{},
		BytesDown: m.bytesDown,
		BytesUp:   m.bytesUp,
		PeerBytes: []PeerBytes{},
	}
	if m.running {
		for _, source := range m.peers {
			if source != "" {
				s.Peers = append(s.Peers, source)
			}
		}
	}
	sort.Strings(s.Peers)
	for ip, t := range m.peerBytes {
		s.PeerBytes = append(s.PeerBytes, PeerBytes{IP: ip, Down: t.down, Up: t.up})
	}
	sort.Slice(s.PeerBytes, func(i, j int) bool { return s.PeerBytes[i].IP < s.PeerBytes[j].IP })
	return s
}

func (m *unboundedManager) recordConnection(ctx context.Context, state, slot int, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	// Prune before acquiring, so a returning IP's fresh tally can't pick up
	// bytes still pending from its pruned lifetime.
	m.prunePeerBytes()
	if state > 0 {
		if m.peers == nil {
			m.peers = make(map[int]string)
		}
		if source != "" {
			known := false
			for _, existing := range m.peers {
				if existing == source {
					known = true
					break
				}
			}
			if !known {
				m.arrivals++
			}
		}
		// Bytes counted before this accept belong to the slot's previous occupant.
		m.foldSlot(slot)
		if prev, ok := m.peers[slot]; ok {
			m.releasePeer(prev)
		}
		delete(m.lastOwner, slot)
		m.peers[slot] = source
		m.acquirePeer(source)
	} else if state < 0 {
		m.foldSlot(slot)
		if prev, ok := m.peers[slot]; ok {
			m.releasePeer(prev)
			// broflake batches upstream bytes for up to 250ms and reports the
			// close without flushing them, so the closed peer keeps the slot's
			// bytes until the next accept.
			if m.lastOwner == nil {
				m.lastOwner = make(map[int]string)
			}
			m.lastOwner[slot] = prev
		}
		delete(m.peers, slot)
	}
}

// retiredRun keeps a stopped run's slot table and its final slot owners, so
// bytes broflake delivers during its asynchronous teardown are still counted
// and credited to the consumers that relayed them.
type retiredRun struct {
	slots     *slotTable
	owners    map[int]string
	retiredAt time.Time
}

// releaseAllPeers folds outstanding slot bytes, ends every live peer so their
// final totals stay visible for peerRetention, and retires the run's slot
// table for the same period. Caller must hold m.mu.
func (m *unboundedManager) releaseAllPeers() {
	m.prunePeerBytes()
	for _, source := range m.peers {
		m.releasePeer(source)
	}
	if m.slots != nil {
		owners := make(map[int]string, len(m.lastOwner)+len(m.peers))
		for idx, source := range m.lastOwner {
			owners[idx] = source
		}
		for idx, source := range m.peers {
			owners[idx] = source
		}
		m.retired = append(m.retired, retiredRun{slots: m.slots, owners: owners, retiredAt: m.clock()})
	}
	m.slots = nil
	m.lastOwner = nil
}

// foldRetiredRuns folds bytes stopped runs received since the last fold, and
// drops runs retired longer than peerRetention after a final fold. Caller
// must hold m.mu.
func (m *unboundedManager) foldRetiredRuns() {
	now := m.clock()
	kept := m.retired[:0]
	for _, r := range m.retired {
		expired := now.Sub(r.retiredAt) > peerRetention
		if expired {
			r.slots.close()
		}
		r.slots.slots.Range(func(k, v any) bool {
			m.addSlotBytes(r.owners[k.(int)], v.(*slotBytes))
			return true
		})
		if !expired {
			kept = append(kept, r)
		}
	}
	clear(m.retired[len(kept):])
	m.retired = kept
}

// foldSlot moves a slot's pending bytes into the totals and, when the slot's
// IP is known, that IP's tally. Caller must hold m.mu.
func (m *unboundedManager) foldSlot(idx int) {
	if m.slots == nil {
		return
	}
	if s, ok := m.slots.slots.Load(idx); ok {
		m.addSlotBytes(m.slotOwner(idx), s.(*slotBytes))
	}
}

func (m *unboundedManager) foldAllSlots() {
	if m.slots == nil {
		return
	}
	m.slots.slots.Range(func(k, v any) bool {
		m.addSlotBytes(m.slotOwner(k.(int)), v.(*slotBytes))
		return true
	})
}

// slotOwner is the IP credited with a live slot's bytes: its current consumer,
// or the last one while the slot is vacant.
func (m *unboundedManager) slotOwner(idx int) string {
	if owner, live := m.peers[idx]; live {
		return owner
	}
	return m.lastOwner[idx]
}

func (m *unboundedManager) addSlotBytes(owner string, s *slotBytes) {
	down, up := s.down.Swap(0), s.up.Swap(0)
	if down == 0 && up == 0 {
		return
	}
	m.bytesDown += down
	m.bytesUp += up
	if t := m.peerBytes[owner]; t != nil {
		t.down += down
		t.up += up
	}
}

func (m *unboundedManager) acquirePeer(source string) {
	if source == "" {
		return
	}
	if m.peerBytes == nil {
		m.peerBytes = make(map[string]*peerTally)
	}
	t := m.peerBytes[source]
	if t == nil {
		t = &peerTally{}
		m.peerBytes[source] = t
	}
	t.active++
}

func (m *unboundedManager) releasePeer(source string) {
	t := m.peerBytes[source]
	if t == nil || t.active == 0 {
		return
	}
	t.active--
	if t.active == 0 {
		t.endedAt = m.clock()
	}
}

// prunePeerBytes drops tallies of IPs ended more than peerRetention ago, and
// expired retired runs. It first folds every pending byte, so bytes are
// always credited to the tally lifetime they were relayed in. Caller must
// hold m.mu.
func (m *unboundedManager) prunePeerBytes() {
	m.foldAllSlots()
	m.foldRetiredRuns()
	now := m.clock()
	for ip, t := range m.peerBytes {
		if t.active == 0 && now.Sub(t.endedAt) > peerRetention {
			delete(m.peerBytes, ip)
		}
	}
}

func (m *unboundedManager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}
