package vpn

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/sagernet/sing-box/experimental/libbox"
)

// sharedPlatform is a platform interface whose default interface monitor several sing-box
// boxes can run at once: the VPN tunnel's and the Unbounded direct exit's. The platform keeps
// one monitor listener (Android's is a single global one), so sharedPlatform registers its own
// and fans each update out to every box's. Everything else passes through.
//
// Listeners may start or close monitors from an update, except from one the platform delivers
// inside its own start or close call.
type sharedPlatform struct {
	libbox.PlatformInterface
	fanOut libbox.InterfaceUpdateListener

	// opMu orders the platform's monitor start and close. started is guarded by it.
	opMu    sync.Mutex
	started bool

	mu        sync.Mutex
	listeners []*monitorListener
	seq       uint64
	// last is the latest default interface update, nil before one or once the monitor closes.
	last *interfaceUpdate
}

// newSharedPlatform wraps p, or returns nil for a nil p.
func newSharedPlatform(p libbox.PlatformInterface) libbox.PlatformInterface {
	if p == nil {
		return nil
	}
	s := &sharedPlatform{PlatformInterface: p}
	s.fanOut = platformFanOut{s}
	return s
}

type interfaceUpdate struct {
	name                       string
	index                      int32
	isExpensive, isConstrained bool
}

// monitorListener is one box's listener. seq is the latest update delivered to it, so a replay
// never lands after a newer update.
type monitorListener struct {
	l      libbox.InterfaceUpdateListener
	closed atomic.Bool

	mu  sync.Mutex
	seq uint64
}

func (m *monitorListener) deliver(seq uint64, u interfaceUpdate) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if seq <= m.seq || m.closed.Load() {
		return
	}
	m.seq = seq
	m.l.UpdateDefaultInterface(u.name, u.index, u.isExpensive, u.isConstrained)
}

// StartDefaultInterfaceMonitor adds listener, starting the platform's monitor for the first.
// A later listener gets the latest update at once, as the platform gives a new listener.
func (s *sharedPlatform) StartDefaultInterfaceMonitor(listener libbox.InterfaceUpdateListener) error {
	entry := &monitorListener{l: listener}
	s.opMu.Lock()
	s.mu.Lock()
	if !s.started {
		// An update can land after the last close. A fresh monitor waits for the platform's.
		s.last = nil
	}
	s.listeners = append(s.listeners, entry)
	seq, last := s.seq, s.last
	s.mu.Unlock()
	if !s.started {
		if err := s.PlatformInterface.StartDefaultInterfaceMonitor(s.fanOut); err != nil {
			s.removeListener(listener)
			s.opMu.Unlock()
			return err
		}
		s.started = true
		last = nil
	}
	s.opMu.Unlock()
	if last != nil {
		entry.deliver(seq, *last)
	}
	return nil
}

// CloseDefaultInterfaceMonitor removes listener, closing the platform's monitor with the last.
func (s *sharedPlatform) CloseDefaultInterfaceMonitor(listener libbox.InterfaceUpdateListener) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if !s.removeListener(listener) {
		return nil
	}
	s.mu.Lock()
	empty := len(s.listeners) == 0
	if empty {
		s.last = nil
	}
	s.mu.Unlock()
	if !empty || !s.started {
		return nil
	}
	s.started = false
	return s.PlatformInterface.CloseDefaultInterfaceMonitor(s.fanOut)
}

func (s *sharedPlatform) removeListener(listener libbox.InterfaceUpdateListener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.listeners, func(m *monitorListener) bool { return m.l == listener })
	if i < 0 {
		return false
	}
	s.listeners[i].closed.Store(true)
	s.listeners = slices.Delete(s.listeners, i, i+1)
	return true
}

// platformFanOut is sharedPlatform's one listener on the platform's monitor.
type platformFanOut struct{ s *sharedPlatform }

func (f platformFanOut) UpdateDefaultInterface(name string, index int32, isExpensive, isConstrained bool) {
	u := interfaceUpdate{name: name, index: index, isExpensive: isExpensive, isConstrained: isConstrained}
	f.s.mu.Lock()
	if len(f.s.listeners) == 0 {
		// The monitor closed after the platform sent this.
		f.s.mu.Unlock()
		return
	}
	f.s.seq++
	seq := f.s.seq
	f.s.last = &u
	listeners := slices.Clone(f.s.listeners)
	f.s.mu.Unlock()
	for _, m := range listeners {
		m.deliver(seq, u)
	}
}

func (f platformFanOut) UpdateNetworkPath(networkPath string) {
	f.s.mu.Lock()
	listeners := slices.Clone(f.s.listeners)
	f.s.mu.Unlock()
	for _, m := range listeners {
		if !m.closed.Load() {
			m.l.UpdateNetworkPath(networkPath)
		}
	}
}
