package memmon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type noopExecutor struct{}

func (noopExecutor) Apply(Decision, time.Time) {}

func TestRunTicksThenStopsOnCancel(t *testing.T) {
	fs := &fakeSampler{pressure: 0.1} // Normal level → reschedule at BaseInterval
	mon := New(Config{BaseInterval: time.Millisecond}, fs, noopExecutor{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		mon.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool { return fs.sampleCount() >= 3 }, time.Second, time.Millisecond,
		"Run samples on each timer tick and reschedules")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
