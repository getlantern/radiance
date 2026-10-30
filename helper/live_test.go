package helper

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type liveClock struct{ t time.Time }

func (c *liveClock) now() time.Time          { return c.t }
func (c *liveClock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newLiveClock() *liveClock               { return &liveClock{t: time.Unix(1_800_000_000, 0)} }
func liveOf(s LiveSnapshot, id string) *LivePairing {
	for i := range s.Pairings {
		if s.Pairings[i].PairingID == id {
			return &s.Pairings[i]
		}
	}
	return nil
}

func TestLive_BelowThresholdIsNotConnected(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	var n atomic.Uint64
	lt.Open("peer", "c1", "pA", n.Load)
	// A url-test probe's worth of bytes every few seconds stays under 32 KiB
	// in any minute.
	for range 20 {
		n.Add(1000)
		clk.advance(3 * time.Second)
		p := liveOf(lt.Snapshot(), "pA")
		require.NotNil(t, p, "an open connection is listed")
		assert.False(t, p.Connected)
	}
}

func TestLive_ThresholdWindowAndLinger(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	var n atomic.Uint64
	lt.Open("peer", "c1", "pA", n.Load)

	n.Add(LiveThreshold - 1)
	clk.advance(time.Second)
	assert.False(t, liveOf(lt.Snapshot(), "pA").Connected, "one byte short")

	n.Add(1)
	clk.advance(time.Second)
	start := clk.t
	p := liveOf(lt.Snapshot(), "pA")
	assert.True(t, p.Connected, "at the threshold")
	assert.Equal(t, start.UnixMilli(), p.Since)

	// The bytes stay in the window for 60s, so it stays connected without
	// more traffic.
	clk.advance(57 * time.Second)
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected)

	// Out of the window: quiet, but lingers.
	clk.advance(5 * time.Second)
	p = liveOf(lt.Snapshot(), "pA")
	assert.True(t, p.Connected, "lingers")
	assert.Equal(t, start.UnixMilli(), p.Since)
	clk.advance(LiveLinger - time.Second)
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected, "still lingering")
	clk.advance(time.Second)
	p = liveOf(lt.Snapshot(), "pA")
	assert.False(t, p.Connected, "linger over")
	assert.Equal(t, clk.t.UnixMilli(), p.Since)
}

func TestLive_LingerAbsorbsAShortDip(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	lt.Add("pA", LiveThreshold)
	since := clk.t.UnixMilli()
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected)
	clk.advance(61 * time.Second)
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected, "dip below")
	clk.advance(5 * time.Second)
	lt.Add("pA", LiveThreshold)
	clk.advance(LiveLinger)
	p := liveOf(lt.Snapshot(), "pA")
	assert.True(t, p.Connected, "traffic back within the linger")
	assert.Equal(t, since, p.Since, "no flap")
}

func TestLive_MultiplePairingsAreIndependent(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	var a, b atomic.Uint64
	lt.Open("peer", "a", "pA", a.Load)
	lt.Open("unbounded", "b", "pB", b.Load)
	lt.Add("pC", 10)
	a.Add(LiveThreshold)
	b.Add(100)
	clk.advance(time.Second)
	s := lt.Snapshot()
	require.Len(t, s.Pairings, 3)
	assert.Equal(t, []string{"pA", "pB", "pC"}, []string{s.Pairings[0].PairingID, s.Pairings[1].PairingID, s.Pairings[2].PairingID})
	assert.True(t, liveOf(s, "pA").Connected)
	assert.False(t, liveOf(s, "pB").Connected)
	assert.False(t, liveOf(s, "pC").Connected)
}

func TestLive_BytesFromSeveralConnectionsAndAddSum(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	var a, b atomic.Uint64
	lt.Open("peer", "a", "pA", a.Load)
	lt.Open("peer", "b", "pA", b.Load)
	a.Add(LiveThreshold / 4)
	b.Add(LiveThreshold / 4)
	lt.Add("pA", LiveThreshold/4)
	clk.advance(time.Second)
	assert.False(t, liveOf(lt.Snapshot(), "pA").Connected)
	lt.Add("pA", LiveThreshold/4)
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected)
}

func TestLive_CloseCountsFinalBytesAndPrunes(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	var n atomic.Uint64
	lt.Open("peer", "c1", "pA", n.Load)
	n.Add(LiveThreshold)
	lt.Close("c1")
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected, "the closed connection's bytes count")
	clk.advance(LiveWindow + time.Second)
	assert.True(t, liveOf(lt.Snapshot(), "pA").Connected, "lingers")
	clk.advance(LiveLinger)
	assert.Nil(t, liveOf(lt.Snapshot(), "pA"), "a disconnected pairing with nothing open is dropped")
}

func TestLive_CloseOwnerOnlyClosesThatOwner(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	lt.Open("peer", "a", "pA", nil)
	lt.Open("unbounded", 1, "pB", nil)
	lt.CloseOwner("peer")
	s := lt.Snapshot()
	assert.Nil(t, liveOf(s, "pA"))
	assert.NotNil(t, liveOf(s, "pB"))
	lt.Close(1)
	assert.Empty(t, lt.Snapshot().Pairings)
}

func TestLive_IgnoresPoolAndReopen(t *testing.T) {
	clk := newLiveClock()
	lt := NewLiveTracker(clk.now)
	lt.Open("peer", "a", "", nil)
	lt.Add("", LiveThreshold)
	assert.Empty(t, lt.Snapshot().Pairings, "pool connections are not tracked")

	lt.Open("peer", "a", "pA", nil)
	lt.Open("peer", "a", "pB", nil) // same key reused: the first is closed
	s := lt.Snapshot()
	assert.Nil(t, liveOf(s, "pA"))
	assert.NotNil(t, liveOf(s, "pB"))
}

func TestLive_NilTrackerIsSafe(t *testing.T) {
	var lt *LiveTracker
	lt.Open("peer", "a", "pA", nil)
	lt.Add("pA", 1)
	lt.Close("a")
	lt.CloseOwner("peer")
	assert.Empty(t, lt.Snapshot().Pairings)
}
