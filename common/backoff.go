package common

import (
	"context"
	"math/rand/v2"
	"time"
)

const defaultBaseWait = 10 * time.Millisecond

// Backoff implements a quadratic backoff strategy with jitter. Should not be used by multiple
// goroutines concurrently.
type Backoff struct {
	n        int // number of consecutive failures
	baseWait time.Duration
	maxWait  time.Duration
}

// NewBackoff creates a new Backoff with the given base wait time and maximum wait time.
// If baseWait is zero or negative, a default value of 10ms is used.
func NewBackoff(baseWait, maxWait time.Duration) *Backoff {
	if baseWait <= 0 {
		baseWait = defaultBaseWait
	}
	return &Backoff{
		baseWait: baseWait,
		maxWait:  maxWait,
	}
}

// Wait pauses for the next backoff interval or returns early if the context is done.
func (b *Backoff) Wait(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	b.n++
	wait := b.baseWait * time.Duration(b.n*b.n)

	// add jitter between 80% and 120% of wait time to avoid thundering herd
	jitter := 0.8 + 0.4*rand.Float64()
	wait = min(b.maxWait, time.Duration(float64(wait)*jitter))
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// Reset resets the backoff counter.
func (b *Backoff) Reset() {
	b.n = 0
}
