package peer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/events"
)

func TestClassifyPort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dest string
		want string
	}{
		{"smtp 25", "smtp.example.com:25", "smtp"},
		{"smtps 465", "smtp.example.com:465", "smtp"},
		{"submission 587", "smtp.example.com:587", "smtp"},
		{"alt smtp 2525", "smtp.example.com:2525", "smtp"},
		{"https 443", "example.com:443", "https"},
		{"https 8443", "example.com:8443", "https"},
		{"http 80", "example.com:80", "http"},
		{"http 8080", "example.com:8080", "http"},
		{"http 8000", "example.com:8000", "http"},
		{"dns 53", "1.1.1.1:53", "dns"},
		{"dns 853", "1.1.1.1:853", "dns"},
		{"irc 6667", "irc.example.com:6667", "irc"},
		{"irc 6697", "irc.example.com:6697", "irc"},
		{"irc range 6601", "irc.example.com:6601", "irc"},
		{"other 21 ftp", "ftp.example.com:21", "other"},
		{"other 22 ssh", "ssh.example.com:22", "other"},
		{"other unknown high", "example.com:54321", "other"},
		{"empty destination", "", "other"},
		{"hostname only no port", "example.com", "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, classifyPort(tc.dest))
		})
	}
}

func TestStripPort(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "10.0.0.1", stripPort("10.0.0.1:443"))
	assert.Equal(t, "::1", stripPort("[::1]:443"))
	// best-effort: no port → return input unchanged
	assert.Equal(t, "10.0.0.1", stripPort("10.0.0.1"))
	assert.Equal(t, "", stripPort(""))
}

func TestAbuseAggregator_NoteAndFlush(t *testing.T) {
	t.Parallel()
	a := newAbuseAggregator(time.Hour) // long flush; we'll call flush manually

	a.note("10.0.0.1:5555", "smtp.botnet.example:25")
	a.note("10.0.0.1:5556", "smtp.botnet.example:25")
	a.note("10.0.0.1:5557", "example.com:443")
	a.note("203.0.113.99:5555", "example.com:443")

	evt := a.flush()
	require.NotNil(t, evt, "flush with activity should return an event")
	require.Len(t, evt.Sources, 2, "expected one bucket per unique source IP")

	// Sources are alphabetically sorted; 10.x < 203.x.
	assert.Equal(t, "10.0.0.1", evt.Sources[0].SourceIP)
	assert.Equal(t, 3, evt.Sources[0].TotalAccepts)
	assert.Equal(t, 2, evt.Sources[0].PortClassCounts["smtp"])
	assert.Equal(t, 1, evt.Sources[0].PortClassCounts["https"])

	assert.Equal(t, "203.0.113.99", evt.Sources[1].SourceIP)
	assert.Equal(t, 1, evt.Sources[1].TotalAccepts)
	assert.Equal(t, 1, evt.Sources[1].PortClassCounts["https"])
}

func TestAbuseAggregator_FlushIsIdempotentRoll(t *testing.T) {
	t.Parallel()
	a := newAbuseAggregator(time.Hour)
	a.note("10.0.0.1:5555", "smtp.example:25")

	first := a.flush()
	require.NotNil(t, first)
	// Second flush with no activity since: nil event so we don't pollute
	// the event stream with empties.
	second := a.flush()
	assert.Nil(t, second, "flush with no activity since last flush should be nil")
}

func TestAbuseAggregator_EmptyFlushReturnsNil(t *testing.T) {
	t.Parallel()
	a := newAbuseAggregator(time.Hour)
	assert.Nil(t, a.flush(), "fresh aggregator with no notes flushes nil")
}

func TestAbuseAggregator_NoteIgnoresBadInput(t *testing.T) {
	t.Parallel()
	a := newAbuseAggregator(time.Hour)
	// Empty source: no signal, must not produce a bucket.
	a.note("", "example.com:443")
	assert.Nil(t, a.flush(), "empty source should be dropped, not emitted")
}

func TestAbuseAggregator_RunFlushLoopEmitsViaEventBus(t *testing.T) {
	// Subscribe to AbuseSummaryEvent before starting the loop.
	got := make(chan AbuseSummaryEvent, 4)
	sub := events.Subscribe(func(evt AbuseSummaryEvent) {
		got <- evt
	})
	defer sub.Unsubscribe()

	a := newAbuseAggregator(50 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.runFlushLoop(ctx)
	}()

	a.note("10.0.0.1:5555", "smtp.example:25")
	a.note("10.0.0.1:5556", "irc.example:6667")

	select {
	case evt := <-got:
		require.Len(t, evt.Sources, 1)
		assert.Equal(t, "10.0.0.1", evt.Sources[0].SourceIP)
		assert.Equal(t, 2, evt.Sources[0].TotalAccepts)
		assert.Equal(t, 1, evt.Sources[0].PortClassCounts["smtp"])
		assert.Equal(t, 1, evt.Sources[0].PortClassCounts["irc"])
	case <-time.After(time.Second):
		t.Fatal("no AbuseSummaryEvent within 1s")
	}

	cancel()
	wg.Wait()
}

func TestAbuseAggregator_FlushLoopEmitsFinalSummaryOnCancel(t *testing.T) {
	got := make(chan AbuseSummaryEvent, 4)
	sub := events.Subscribe(func(evt AbuseSummaryEvent) {
		got <- evt
	})
	defer sub.Unsubscribe()

	// Long interval — only the cancel-time final flush should fire.
	a := newAbuseAggregator(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		a.runFlushLoop(ctx)
	}()

	a.note("10.0.0.1:5555", "example.com:443")
	cancel()
	wg.Wait()

	// events.Emit delivers asynchronously (one goroutine per subscriber)
	// so wg.Wait returning doesn't guarantee the event has landed in
	// our channel yet. Brief wait covers the dispatch goroutine.
	select {
	case evt := <-got:
		require.Len(t, evt.Sources, 1)
		assert.Equal(t, "10.0.0.1", evt.Sources[0].SourceIP)
	case <-time.After(time.Second):
		t.Fatal("expected a final summary on cancel within 1s; got none")
	}
}
