package log

import (
	"sync"
)

// LogEntry is a formatted log line streamed to clients.
type LogEntry = string

// Subscribe returns a channel that receives log entries from the default logger
// and an unsubscribe function. Recent entries from the ring buffer are sent
// immediately.
func Subscribe() (chan LogEntry, func()) {
	return defaultPublisher.subscribe()
}

var defaultPublisher = newPublisher(200)

// Publisher returns the default log publisher. Include it in the handler's
// writer chain so published entries share the same format.
func Publisher() *publisher {
	return defaultPublisher
}

// publisher fans out log lines to connected SSE clients. It implements io.Writer
// so it can be included in the handler's writer chain. It maintains a ring buffer
// of recent entries so new subscribers get immediate context.
type publisher struct {
	clients  map[chan LogEntry]struct{}
	ring     []LogEntry
	ringSize int
	ringIdx  int
	mu       sync.RWMutex
}

func newPublisher(ringSize int) *publisher {
	return &publisher{
		clients:  make(map[chan LogEntry]struct{}),
		ring:     make([]LogEntry, ringSize),
		ringSize: ringSize,
	}
}

// Write implements io.Writer. Each call is treated as a single log line.
func (lb *publisher) Write(b []byte) (int, error) {
	entry := string(b)
	lb.publish(entry)
	return len(b), nil
}

func (lb *publisher) publish(entry LogEntry) {
	lb.mu.Lock()
	lb.ring[lb.ringIdx%lb.ringSize] = entry
	lb.ringIdx++
	lb.mu.Unlock()

	lb.mu.RLock()
	defer lb.mu.RUnlock()
	for ch := range lb.clients {
		select {
		case ch <- entry:
		default: // drop if client is slow
		}
	}
}

func (lb *publisher) subscribe() (chan LogEntry, func()) {
	ch := make(chan LogEntry, lb.ringSize)
	lb.mu.Lock()
	start := max(0, lb.ringIdx-lb.ringSize)
	for i := start; i < lb.ringIdx; i++ {
		entry := lb.ring[i%lb.ringSize]
		if entry != "" {
			ch <- entry
		}
	}
	lb.clients[ch] = struct{}{}
	lb.mu.Unlock()

	unsub := func() {
		lb.mu.Lock()
		delete(lb.clients, ch)
		lb.mu.Unlock()
	}
	return ch, unsub
}
