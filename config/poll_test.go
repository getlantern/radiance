package config

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

const testPollInterval = 3 * time.Minute

type countingFetcher struct {
	calls atomic.Int32
	err   error
}

func (f *countingFetcher) fetchConfig(context.Context, common.PreferredLocation, string) ([]byte, error) {
	f.calls.Add(1)
	return nil, f.err
}

// startPollLoop runs fetchLoop in the current synctest bubble and waits for its
// first fetch. The loop stops when the test ends.
func startPollLoop(t *testing.T, f *countingFetcher, paused bool) *ConfigHandler {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	ch := &ConfigHandler{
		ftr:          f,
		ctx:          ctx,
		cancel:       cancel,
		logger:       log.NoOpLogger(),
		pauseChanged: make(chan struct{}, 1),
		configPath:   filepath.Join(dir, internal.ConfigFileName),
		wgKeyPath:    filepath.Join(dir, "wg.key"),
	}
	ch.devicePaused.Store(paused)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ch.fetchLoop(testPollInterval)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	synctest.Wait()
	require.EqualValues(t, 1, f.calls.Load(), "the loop fetches once on start")
	return ch
}

// advance moves the bubble's clock by d and lets the loop react.
func advance(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func TestFetchLoopWaitsPausedIntervalWhileDevicePaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &countingFetcher{}
		ch := startPollLoop(t, f, false)
		ch.SetDevicePaused(true)

		advance(pausedPollInterval - time.Second)
		assert.EqualValues(t, 1, f.calls.Load(), "no fetch before pausedPollInterval while paused")
		advance(time.Second)
		assert.EqualValues(t, 2, f.calls.Load(), "fetches once pausedPollInterval has passed")
	})
}

func TestFetchLoopRechecksPauseWhenTimerFires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &countingFetcher{}
		ch := startPollLoop(t, f, false)
		// A pause whose signal loses the race to the timer.
		ch.devicePaused.Store(true)

		advance(testPollInterval)
		assert.EqualValues(t, 1, f.calls.Load(), "the timer must not fetch on the normal interval while paused")
		advance(pausedPollInterval - testPollInterval)
		assert.EqualValues(t, 2, f.calls.Load())
	})
}

func TestFetchLoopFetchesOnWakeWhenOverdue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &countingFetcher{}
		ch := startPollLoop(t, f, false)
		ch.SetDevicePaused(true)

		advance(10 * time.Minute)
		require.EqualValues(t, 1, f.calls.Load())
		ch.SetDevicePaused(false)
		synctest.Wait()
		assert.EqualValues(t, 2, f.calls.Load(), "a fetch that came due while paused runs on wake")

		advance(testPollInterval)
		assert.EqualValues(t, 3, f.calls.Load(), "the normal interval resumes after the wake fetch")
	})
}

func TestFetchLoopKeepsScheduleOnWakeBeforeDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &countingFetcher{}
		ch := startPollLoop(t, f, false)
		ch.SetDevicePaused(true)

		advance(time.Minute)
		ch.SetDevicePaused(false)
		synctest.Wait()
		assert.EqualValues(t, 1, f.calls.Load(), "a wake before the fetch is due does not fetch")

		advance(testPollInterval - time.Minute)
		assert.EqualValues(t, 2, f.calls.Load(), "fetches on the original schedule")
	})
}

func TestFetchLoopFailedFetchWaitsForWakeWhilePaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &countingFetcher{err: errors.New("offline")}
		ch := startPollLoop(t, f, true)

		advance(30 * time.Minute)
		assert.EqualValues(t, 1, f.calls.Load(), "a paused failure is not retried on the backoff schedule")
		ch.SetDevicePaused(false)
		synctest.Wait()
		assert.EqualValues(t, 2, f.calls.Load(), "a paused failure is retried on wake")
	})
}

func TestFetchLoopBackoffYieldsToPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &countingFetcher{err: errors.New("offline")}
		ch := startPollLoop(t, f, false)
		ch.SetDevicePaused(true)

		advance(pausedPollInterval - time.Second)
		assert.EqualValues(t, 1, f.calls.Load(), "a pause during backoff defers the retry")
		advance(time.Second)
		assert.EqualValues(t, 2, f.calls.Load())
	})
}
