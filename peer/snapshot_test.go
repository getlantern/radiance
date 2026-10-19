package peer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/lantern-box/tracker/peerconn"
)

func counter(down, up int) *peerconn.Counter {
	c := &peerconn.Counter{}
	c.AddDown(down)
	c.AddUp(up)
	return c
}

// Open connections are read live; a close folds its final count so the totals
// keep it, and a client's connections add up under its IP.
func TestRelayStats_CountsLiveAndClosedConnections(t *testing.T) {
	var r relayStats
	r.begin()
	a, b := counter(100, 10), counter(50, 5)
	r.record(1, "203.0.113.7:1000", a)
	r.record(1, "203.0.113.7:1001", b)
	other := counter(7, 3)
	r.record(1, "198.51.100.9:2000", other)

	a.AddDown(900)
	s := r.snapshot(true)
	assert.Equal(t, uint64(1057), s.BytesDown)
	assert.Equal(t, uint64(18), s.BytesUp)
	assert.Equal(t, []string{"198.51.100.9:2000", "203.0.113.7:1000", "203.0.113.7:1001"}, s.Peers)
	assert.Equal(t, []PeerBytes{
		{IP: "198.51.100.9", Down: 7, Up: 3},
		{IP: "203.0.113.7", Down: 1050, Up: 15},
	}, s.PeerBytes)

	r.record(-1, "203.0.113.7:1000", a)
	a.AddDown(1) // the counter is no longer read once its close is folded
	s = r.snapshot(true)
	assert.Equal(t, uint64(1057), s.BytesDown, "a closed connection's bytes stay counted")
	assert.Equal(t, []PeerBytes{
		{IP: "198.51.100.9", Down: 7, Up: 3},
		{IP: "203.0.113.7", Down: 1050, Up: 15},
	}, s.PeerBytes)
	assert.NotContains(t, s.Peers, "203.0.113.7:1000")
}

// samizdat multiplexes streams over one TCP conn, so two open streams can share
// a source; each keeps its own count, and closing one leaves the other live.
func TestRelayStats_StreamsSharingASource(t *testing.T) {
	var r relayStats
	r.begin()
	a, b := counter(100, 1), counter(200, 2)
	r.record(1, "203.0.113.7:1000", a)
	r.record(1, "203.0.113.7:1000", b)
	assert.Equal(t, uint64(300), r.snapshot(true).BytesDown)

	r.record(-1, "203.0.113.7:1000", a)
	b.AddDown(50)
	s := r.snapshot(true)
	assert.Equal(t, uint64(350), s.BytesDown, "the open stream is still read live")
	assert.Equal(t, []PeerBytes{{IP: "203.0.113.7", Down: 350, Up: 3}}, s.PeerBytes)
	assert.Equal(t, []string{"203.0.113.7:1000"}, s.Peers)
}

// A departed client stays in the snapshot long enough for the app's poller to
// see its final count, then drops out; session totals keep its bytes.
func TestRelayStats_PrunesDepartedClientsAfterRetention(t *testing.T) {
	now := time.Unix(1_000, 0)
	r := relayStats{now: func() time.Time { return now }}
	r.begin()
	c := counter(40, 4)
	r.record(1, "203.0.113.7:1000", c)
	r.record(-1, "203.0.113.7:1000", c)

	now = now.Add(relayRetention)
	assert.Len(t, r.snapshot(true).PeerBytes, 1)
	now = now.Add(time.Second)
	s := r.snapshot(true)
	assert.Empty(t, s.PeerBytes)
	assert.Equal(t, uint64(40), s.BytesDown)
}

// Each session counts from zero under its own epoch, but the last session's
// totals stay readable until the next begins.
func TestRelayStats_NewEpochPerSession(t *testing.T) {
	now := time.Unix(1_000, 0)
	r := relayStats{now: func() time.Time { return now }}
	r.begin()
	first := r.snapshot(true).Epoch
	r.record(1, "203.0.113.7:1000", counter(10, 1))
	assert.Equal(t, uint64(10), r.snapshot(false).BytesDown, "stopping keeps the totals")
	assert.Empty(t, r.snapshot(false).Peers, "a stopped session lists no peers")

	now = now.Add(time.Second)
	r.begin()
	s := r.snapshot(true)
	assert.NotEqual(t, first, s.Epoch)
	assert.Zero(t, s.BytesDown)
	assert.Empty(t, s.PeerBytes)
}

// Connections without a counter, and closes with no recorded accept, are
// ignored rather than miscounted.
func TestRelayStats_IgnoresUncountedAndUnmatchedEvents(t *testing.T) {
	var r relayStats
	r.begin()
	r.record(1, "203.0.113.7:1000", nil)
	r.record(-1, "203.0.113.7:1001", counter(5, 5))
	s := r.snapshot(true)
	assert.Zero(t, s.BytesDown)
	assert.Empty(t, s.PeerBytes)
}

// End to end through a started client: counted connections show up in
// RelaySnapshot, and lantern-cloud's verifier does not.
func TestClient_RelaySnapshot(t *testing.T) {
	fwd := &fakeForwarder{externalIP: "203.0.113.42"}
	c := newTestClient(t, fwd, &fakeBoxService{}, newStubServer(t))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx))

	client := counter(0, 0)
	peerconn.Notify(peerconn.Event{State: +1, Source: "5.119.14.105:10304", Destination: "google.com:443", Bytes: client})
	peerconn.Notify(peerconn.Event{State: +1, Source: "35.233.177.182:33792", Destination: "api.iantem.io:443", Bytes: counter(500, 50)})
	client.AddDown(1200)
	client.AddUp(300)

	s := c.RelaySnapshot()
	assert.True(t, s.Running)
	assert.NotEmpty(t, s.Epoch)
	assert.Equal(t, uint64(1200), s.BytesDown)
	assert.Equal(t, uint64(300), s.BytesUp)
	assert.Equal(t, []string{"5.119.14.105:10304"}, s.Peers)
	assert.Equal(t, []PeerBytes{{IP: "5.119.14.105", Down: 1200, Up: 300}}, s.PeerBytes)

	require.NoError(t, c.Stop(ctx))
	s = c.RelaySnapshot()
	assert.False(t, s.Running)
	assert.Equal(t, uint64(1200), s.BytesDown, "a poll after Stop still sees the final bytes")
}
