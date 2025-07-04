package vpn

import (
	"context"
	"time"
)

// mu is a channel-based mutex lock that preserves caller order (FIFO).
// With sync.Mutex, goroutines may acquire the lock in any order depending on the scheduler.
type mu struct {
	ch chan struct{}
}

func newMu() *mu {
	return &mu{
		ch: make(chan struct{}, 1),
	}
}

func (l *mu) Lock() {
	l.ch <- struct{}{}
}

func (l *mu) Unlock() {
	select {
	case <-l.ch:
	default:
	}
}

func (l *mu) TryLock() bool {
	select {
	case l.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// TryLockWithTimeout attempts to acquire the lock within the given timeout.
// Returns true if the lock was acquired before timeout, false otherwise.
func (l *mu) TryLockWithTimeout(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case l.ch <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// LockWithContext attempts to acquire the lock, blocking until either the lock is acquired
// or the context is canceled or times out. Returns an error if context ends first.
func (l *mu) LockWithContext(ctx context.Context) error {
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
