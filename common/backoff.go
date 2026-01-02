package common

import (
	"context"
	"math/rand/v2"
	"time"
)

const waitScale = 10 * time.Millisecond // scale factor for backoff timing

// Backoff implements an exponential backoff strategy with jitter.
type Backoff struct {
	n       int // number of consecutive failures
	maxWait time.Duration
}

func NewBackoff(maxWait time.Duration) *Backoff {
	return &Backoff{
		maxWait: maxWait,
	}
}

// Wait waits for the appropriate backoff duration based on the number of consecutive failures or
// until the context is done.
func (b *Backoff) Wait(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	b.n++
	// exponential backoff: waitScale * n^2, capped at maxWait
	wait := waitScale * time.Duration(b.n*b.n)
	wait = min(wait, b.maxWait)

	// add jitter between 80% and 120% of wait time to avoid thundering herd
	jitter := 0.8 + 0.4*rand.Float64()
	wait = time.Duration(float64(wait) * jitter)
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

// Reset resets the backoff counter.
func (b *Backoff) Reset() {
	b.n = 0
}
