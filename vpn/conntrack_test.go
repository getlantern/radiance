package vpn

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingObserver struct {
	mu     sync.Mutex
	opens  []ConnAttrs
	closes []ConnClose
}

func (o *recordingObserver) OnOpen(a ConnAttrs) {
	o.mu.Lock()
	o.opens = append(o.opens, a)
	o.mu.Unlock()
}

func (o *recordingObserver) OnClose(c ConnClose) {
	o.mu.Lock()
	o.closes = append(o.closes, c)
	o.mu.Unlock()
}

// wrapTCP mirrors clashServer.RoutedConnection's counter wiring so byte counting can be exercised
// without a full tunnel.
func wrapTCP(ct *connTracker, r *record, conn net.Conn) *tcpConn {
	c := &tcpConn{
		ExtendedConn: bufio.NewCounterConn(conn, []N.CountFunc{func(n int64) {
			r.upload.Add(n)
			ct.pushUploaded(n)
		}}, []N.CountFunc{func(n int64) {
			r.download.Add(n)
			ct.pushDownloaded(n)
		}}),
		rec: r,
		ct:  ct,
	}
	ct.join(r)
	return c
}

func TestConnTracker_LeaveFoldsOnce(t *testing.T) {
	ct := newConnTracker()
	tr := newThroughputTracker(ct, time.Second)
	ct.tp = tr

	r := newRec("out")
	ct.join(r)
	addBytes(ct, r, 100, 50)

	ct.leave(r)
	ct.leave(r) // repeat close must be a no-op

	_, ok := ct.conns.Load(r.id)
	assert.False(t, ok, "record should be removed from the active set")
	require.Len(t, tr.pending, 1, "final bytes folded exactly once")
	assert.Equal(t, closedDelta{id: r.id, outbound: "out", up: 100, down: 50}, tr.pending[0])
}

func TestConnTracker_ObserverOpenClose(t *testing.T) {
	ct := newConnTracker()
	obs := &recordingObserver{}
	ct.SetObserver(obs)

	r := newRec("vpn-a")
	r.createdAt = time.Now().Add(-2 * time.Second)
	r.outboundType = "vmess"
	r.inboundType, r.inboundName = "tun", "tun-in"

	ct.join(r)
	addBytes(ct, r, 300, 120)
	ct.leave(r)

	require.Len(t, obs.opens, 1)
	require.Len(t, obs.closes, 1)
	assert.Equal(t, "vmess/vpn-a", obs.opens[0].Outbound)
	assert.Equal(t, "tun/tun-in", obs.opens[0].Inbound)
	c := obs.closes[0]
	assert.Equal(t, int64(300), c.Uplink)
	assert.Equal(t, int64(120), c.Downlink)
	assert.InDelta(t, 2.0, c.DurationSeconds, 0.5)
}

func TestConnTracker_TotalAndShim(t *testing.T) {
	ct := newConnTracker()
	a, b := newRec("vpn-a"), newRec("vpn-b")
	a.outboundType = "vmess"
	ct.join(a)
	ct.join(b)
	addBytes(ct, a, 10, 20)
	addBytes(ct, b, 5, 7)

	up, down := ct.Total()
	assert.Equal(t, int64(15), up)
	assert.Equal(t, int64(27), down)

	conns := ct.Connections()
	require.Len(t, conns, 2)
	for _, m := range conns {
		assert.True(t, m.ClosedAt.IsZero(), "active records report a zero ClosedAt")
		assert.NotNil(t, m.Upload)
		assert.NotNil(t, m.Download)
		assert.Contains(t, []string{"vpn-a", "vpn-b"}, m.Outbound)
	}

	count, perOut := ct.activeStats()
	assert.Equal(t, 2, count)
	assert.Equal(t, map[string]int{"vpn-a": 1, "vpn-b": 1}, perOut)

	active := ct.activeConnections()
	require.Len(t, active, 2)
}

func TestConnTracker_CountsBytesAndFoldsOnClose(t *testing.T) {
	ct := newConnTracker()
	tr := newThroughputTracker(ct, time.Second)
	ct.tp = tr

	srv, cli := net.Pipe()
	defer srv.Close()
	r := newRec("out")
	tc := wrapTCP(ct, r, cli)

	msg := []byte("hello, world!")
	done := make(chan struct{})
	go func() {
		buf := make([]byte, len(msg))
		io.ReadFull(srv, buf)
		close(done)
	}()
	n, err := tc.Write(msg)
	require.NoError(t, err)
	require.Equal(t, len(msg), n)
	<-done

	_, down := ct.Total()
	assert.Equal(t, int64(len(msg)), down)
	assert.Equal(t, int64(len(msg)), r.download.Load())

	require.NoError(t, tc.Close())
	_ = tc.Close() // second close: leave is a no-op, inner close may error

	_, ok := ct.conns.Load(r.id)
	assert.False(t, ok)
	require.Len(t, tr.pending, 1)
	assert.Equal(t, int64(len(msg)), tr.pending[0].down)
}

func TestConnTracker_ConcurrentJoinLeave(t *testing.T) {
	ct := newConnTracker()
	tr := newThroughputTracker(ct, time.Second)
	ct.tp = tr

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newRec("out")
			ct.join(r)
			addBytes(ct, r, 1, 1)
			ct.leave(r)
		}()
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ct.Connections()
			_, _ = ct.activeStats()
			ct.Total()
		}()
	}
	wg.Wait()

	count, _ := ct.activeStats()
	assert.Equal(t, 0, count, "all connections left")
	up, down := ct.Total()
	assert.Equal(t, int64(n), up)
	assert.Equal(t, int64(n), down)
}
