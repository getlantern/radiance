package helper

import (
	"sort"
	"sync"
	"time"
)

// Defaults for LiveTracker. A pairing is connected when it moved at least
// LiveThreshold bytes in the last LiveWindow. The threshold is far above what
// a url-test probe or an idle WebRTC slot moves in a minute, so neither shows
// a friend as connected. A connected pairing stays so until it has been below
// the threshold for LiveLinger, so traffic hovering at the threshold does not
// flap.
const (
	LiveThreshold = 32 << 10
	LiveWindow    = 60 * time.Second
	LiveLinger    = 15 * time.Second
)

// DefaultLive is the process's LiveTracker, fed by direct sharing (peer) and
// the Unbounded widget, and read by the backend's HelperLive.
var DefaultLive = NewLiveTracker(nil)

// LivePairing is one pairing's live state on the volunteer.
type LivePairing struct {
	PairingID string `json:"pairing_id"`
	// Connected says the friend is relaying real traffic through this device
	// now.
	Connected bool `json:"connected"`
	// Since is when Connected last changed, or when the pairing was first
	// seen if it never has, in Unix milliseconds.
	Since int64 `json:"since"`
}

// LiveSnapshot is the live state of every pairing with an open connection,
// recent traffic, or a connected state. A pairing missing from it is not
// connected.
type LiveSnapshot struct {
	Pairings []LivePairing `json:"pairings"`
}

// LiveTracker works out, per helper pairing, whether the friend is connected
// to this volunteer right now. It is in memory only: pairing IDs and their
// timeline are never written to disk, logged, or sent anywhere but the local
// app.
type LiveTracker struct {
	mu        sync.Mutex
	now       func() time.Time
	threshold uint64
	window    time.Duration
	linger    time.Duration
	pairings  map[string]*livePairing
	flows     map[any]*liveFlow
}

type liveFlow struct {
	owner   string
	pairing string
	read    func() uint64
	last    uint64
}

type liveSample struct {
	at time.Time
	n  uint64
}

type livePairing struct {
	// samples are byte counts per second, oldest first, within the window.
	samples   []liveSample
	flows     int
	connected bool
	since     time.Time
	// quietSince is when a connected pairing fell below the threshold, or
	// zero while it is above it.
	quietSince time.Time
}

// NewLiveTracker returns a tracker with the default threshold, window and
// linger. now nil means time.Now.
func NewLiveTracker(now func() time.Time) *LiveTracker {
	if now == nil {
		now = time.Now
	}
	return &LiveTracker{
		now:       now,
		threshold: LiveThreshold,
		window:    LiveWindow,
		linger:    LiveLinger,
		pairings:  make(map[string]*livePairing),
		flows:     make(map[any]*liveFlow),
	}
}

// Open starts following a connection that came through pairing. key
// identifies it for Close, owner groups it for CloseOwner, and read, if not
// nil, returns its cumulative byte count, which is sampled at each Snapshot
// and at Close. A connection with no pairing (a pool one) is ignored.
func (t *LiveTracker) Open(owner string, key any, pairing string, read func() uint64) {
	if t == nil || pairing == "" || key == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if old, ok := t.flows[key]; ok {
		t.closeLocked(key, old, now)
	}
	f := &liveFlow{owner: owner, pairing: pairing, read: read}
	if read != nil {
		f.last = read()
	}
	t.flows[key] = f
	p := t.pairingLocked(pairing, now)
	p.flows++
	t.evaluateLocked(p, now)
}

// Close stops following the connection key, counting its last bytes.
func (t *LiveTracker) Close(key any) {
	if t == nil || key == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if f, ok := t.flows[key]; ok {
		t.closeLocked(key, f, t.now())
	}
}

// CloseOwner closes every connection opened with owner, for a source that
// stopped and will report no closes.
func (t *LiveTracker) CloseOwner(owner string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for key, f := range t.flows {
		if f.owner == owner {
			t.closeLocked(key, f, now)
		}
	}
}

// Add counts n bytes relayed for pairing now, for a source that reports
// bytes as they move rather than as a running total.
func (t *LiveTracker) Add(pairing string, n uint64) {
	if t == nil || pairing == "" || n == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	p := t.pairingLocked(pairing, now)
	p.add(now, n)
	t.evaluateLocked(p, now)
}

// Snapshot samples every open connection and returns each pairing's state,
// sorted by pairing ID.
func (t *LiveTracker) Snapshot() LiveSnapshot {
	s := LiveSnapshot{Pairings: []LivePairing{}}
	if t == nil {
		return s
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, f := range t.flows {
		t.sampleLocked(f, now)
	}
	for id, p := range t.pairings {
		t.evaluateLocked(p, now)
		if !p.connected && p.flows == 0 && len(p.samples) == 0 {
			delete(t.pairings, id)
			continue
		}
		s.Pairings = append(s.Pairings, LivePairing{PairingID: id, Connected: p.connected, Since: p.since.UnixMilli()})
	}
	sort.Slice(s.Pairings, func(i, j int) bool { return s.Pairings[i].PairingID < s.Pairings[j].PairingID })
	return s
}

// Reset forgets everything, for tests.
func (t *LiveTracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.pairings)
	clear(t.flows)
}

func (t *LiveTracker) pairingLocked(id string, now time.Time) *livePairing {
	p := t.pairings[id]
	if p == nil {
		p = &livePairing{since: now}
		t.pairings[id] = p
	}
	return p
}

func (t *LiveTracker) closeLocked(key any, f *liveFlow, now time.Time) {
	t.sampleLocked(f, now)
	delete(t.flows, key)
	if p := t.pairings[f.pairing]; p != nil {
		p.flows--
		t.evaluateLocked(p, now)
	}
}

func (t *LiveTracker) sampleLocked(f *liveFlow, now time.Time) {
	if f.read == nil {
		return
	}
	cur := f.read()
	if cur <= f.last {
		return
	}
	n := cur - f.last
	f.last = cur
	t.pairingLocked(f.pairing, now).add(now, n)
}

func (p *livePairing) add(now time.Time, n uint64) {
	sec := now.Truncate(time.Second)
	if last := len(p.samples) - 1; last >= 0 && p.samples[last].at.Equal(sec) {
		p.samples[last].n += n
		return
	}
	p.samples = append(p.samples, liveSample{at: sec, n: n})
}

func (t *LiveTracker) evaluateLocked(p *livePairing, now time.Time) {
	cutoff := now.Add(-t.window)
	drop := 0
	for drop < len(p.samples) && !p.samples[drop].at.After(cutoff) {
		drop++
	}
	if drop > 0 {
		p.samples = append(p.samples[:0], p.samples[drop:]...)
	}
	var total uint64
	for _, s := range p.samples {
		total += s.n
	}
	switch {
	case total >= t.threshold:
		p.quietSince = time.Time{}
		if !p.connected {
			p.connected = true
			p.since = now
		}
	case p.connected:
		if p.quietSince.IsZero() {
			p.quietSince = now
		}
		if now.Sub(p.quietSince) >= t.linger {
			p.connected = false
			p.quietSince = time.Time{}
			p.since = now
		}
	}
}
