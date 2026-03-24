package log

import (
	"context"
	"log/slog"
	"sync"
)

// Subscribe returns a channel that receives log entries from the default logger
// and an unsubscribe function. Recent entries from the ring buffer are sent
// immediately.
func Subscribe() (chan LogEntry, func()) {
	h, ok := slog.Default().Handler().(*PublishHandler)
	if ok {
		return h.Subscribe()
	}
	ph := &PublishHandler{inner: h, publisher: newPublisher(200)}
	slog.SetDefault(slog.New(ph))
	return ph.Subscribe()
}

// LogEntry is a structured log entry streamed to clients.
type LogEntry struct {
	Time    string         `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"msg"`
	Source  string         `json:"source,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// PublishHandler wraps an slog.Handler and broadcasts each record to an observer.
type PublishHandler struct {
	inner     slog.Handler
	publisher *publisher
}

func (h *PublishHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *PublishHandler) Handle(ctx context.Context, record slog.Record) error {
	entry := LogEntry{
		Time:    record.Time.UTC().Format("2006-01-02 15:04:05.000 UTC"),
		Level:   record.Level.String(),
		Message: record.Message,
	}
	if record.NumAttrs() > 0 {
		entry.Attrs = make(map[string]any, record.NumAttrs())
		record.Attrs(func(a slog.Attr) bool {
			entry.Attrs[a.Key] = a.Value.String()
			return true
		})
	}
	h.publisher.publish(entry)
	return h.inner.Handle(ctx, record)
}

func (h *PublishHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &PublishHandler{inner: h.inner.WithAttrs(attrs), publisher: h.publisher}
}

func (h *PublishHandler) WithGroup(name string) slog.Handler {
	return &PublishHandler{inner: h.inner.WithGroup(name), publisher: h.publisher}
}

// Subscribe returns a channel that receives log entries and an unsubscribe function.
// Recent entries from the ring buffer are sent immediately.
func (h *PublishHandler) Subscribe() (chan LogEntry, func()) {
	return h.publisher.subscribe()
}

// publisher fans out log entries to connected SSE clients. It maintains a ring buffer
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
		if entry.Time != "" {
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
