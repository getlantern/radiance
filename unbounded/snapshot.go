package unbounded

import (
	"context"
	"sort"
	"strconv"
	"time"
)

var snapshotEpoch = strconv.FormatInt(time.Now().UnixNano(), 10)

// Snapshot describes the current widget state, including peers missed by event subscribers.
type Snapshot struct {
	Epoch    string   `json:"epoch"`
	Arrivals uint64   `json:"arrivals"`
	Enabled  bool     `json:"enabled"`
	Running  bool     `json:"running"`
	Peers    []string `json:"peers"`
}

// CurrentSnapshot returns an independent copy of the live widget state.
func CurrentSnapshot() Snapshot {
	return manager.snapshot()
}

func (m *unboundedManager) snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Epoch: snapshotEpoch, Arrivals: m.arrivals, Enabled: Enabled(), Running: m.running, Peers: []string{}}
	if m.running {
		for _, source := range m.peers {
			if source != "" {
				s.Peers = append(s.Peers, source)
			}
		}
	}
	sort.Strings(s.Peers)
	return s
}

func (m *unboundedManager) recordConnection(ctx context.Context, state, slot int, source string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
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
		m.peers[slot] = source
	} else if state < 0 {
		delete(m.peers, slot)
	}
}
