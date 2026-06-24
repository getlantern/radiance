package vpn

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	N "github.com/sagernet/sing/common/network"

	lsync "github.com/getlantern/common/sync"
)

// record holds the lean per-connection state radiance actually reads. It is built once when a
// connection is routed and never retains the full adapter.InboundContext; the scalars below are
// copied out at creation. upload/download are mutated on the data path via atomics; every other
// field is immutable after creation.
type record struct {
	id        uuid.UUID
	createdAt time.Time
	upload    atomic.Int64
	download  atomic.Int64

	// closed is the exactly-once gate for leave: Close may fire more than once (half-close,
	// error/abort, ctx-cancel then explicit close), but the accounting must fold only once.
	closed atomic.Bool

	// closer closes the underlying wrapped connection; set once at creation so the memory monitor
	// can evict a specific connection by id. Closing it folds the record out via leave.
	closer io.Closer

	outbound     string // raw leaf outbound tag: per-outbound bucket key and group-manager shim
	outboundType string
	chain        []string

	inboundType  string
	inboundName  string
	ipVersion    uint8
	network      string
	source       string
	destination  string
	domain       string
	protocol     string
	fromOutbound string
	ruleStr      string
}

// attrs returns the telemetry attribute set for the connection.
func (r *record) attrs() ConnAttrs {
	return ConnAttrs{
		ID:           r.id.String(),
		FromOutbound: r.fromOutbound,
		Outbound:     r.outboundType + "/" + r.outbound,
		Inbound:      r.inboundType + "/" + r.inboundName,
		Network:      r.network,
		Protocol:     r.protocol,
		IPVersion:    int(r.ipVersion),
		Rule:         r.ruleStr,
		ChainList:    r.chain,
	}
}

// trackerMetadata synthesizes the upstream metadata view required by the lantern-box group manager
// (groups.ConnectionManager). That consumer reads only Outbound and ClosedAt.IsZero(); ClosedAt is
// left zero because only active records are ever exposed. Upload/Download point at the live atomics
// so any future consumer that converts or marshals the value does not dereference nil.
func (r *record) trackerMetadata() trafficontrol.TrackerMetadata {
	return trafficontrol.TrackerMetadata{
		ID:           r.id,
		CreatedAt:    r.createdAt,
		Upload:       &r.upload,
		Download:     &r.download,
		Chain:        r.chain,
		Outbound:     r.outbound,
		OutboundType: r.outboundType,
	}
}

// ConnAttrs is the attribute set describing a connection, carried on each ConnClose push.
type ConnAttrs struct {
	ID           string
	FromOutbound string
	Outbound     string
	Inbound      string
	Network      string
	Protocol     string
	IPVersion    int
	Rule         string
	ChainList    []string
}

// ConnClose is pushed when a connection closes, carrying its final accounting.
type ConnClose struct {
	ConnAttrs
	DurationSeconds float64
	Uplink          int64
	Downlink        int64
}

// ConnObserver receives a push notification when a connection closes. Implementations must not
// block: OnClose runs on the connection's close goroutine.
type ConnObserver interface {
	OnClose(ConnClose)
}

// connTracker is radiance's connection tracker. It holds only the active set and global byte totals;
// closed connections are pushed to the throughput sampler and the telemetry observer at close rather
// than retained.
type connTracker struct {
	upTotal   atomic.Int64
	downTotal atomic.Int64

	conns       lsync.TypedMap[uuid.UUID, *record]
	activeCount atomic.Int64

	tp *throughputTracker // wired after construction

	obsMu    sync.RWMutex
	observer ConnObserver
}

func newConnTracker() *connTracker { return &connTracker{} }

// SetObserver sets the telemetry observer, or nil to detach. It is re-set on each tunnel connect
// because the tracker is recreated per tunnel while the observer outlives it.
func (m *connTracker) SetObserver(o ConnObserver) {
	m.obsMu.Lock()
	m.observer = o
	m.obsMu.Unlock()
}

func (m *connTracker) currentObserver() ConnObserver {
	m.obsMu.RLock()
	defer m.obsMu.RUnlock()
	return m.observer
}

func (m *connTracker) pushUploaded(n int64)   { m.upTotal.Add(n) }
func (m *connTracker) pushDownloaded(n int64) { m.downTotal.Add(n) }

// Total returns the cumulative up/down byte counters across all connections, including those already
// closed (counting happens on the data path, independent of the active set).
func (m *connTracker) Total() (up, down int64) {
	return m.upTotal.Load(), m.downTotal.Load()
}

func (m *connTracker) join(r *record) {
	m.conns.Store(r.id, r)
	m.activeCount.Add(1)
}

// leave folds the connection's final accounting exactly once, gated by record.closed.
func (m *connTracker) leave(r *record) {
	if !r.closed.CompareAndSwap(false, true) {
		return
	}

	up, down := r.upload.Load(), r.download.Load()
	// Hand the close to the sampler before removing the connection from the active set: a sample
	// tick that misses the connection in its active walk must then find it in the pending drain,
	// never in neither. Were it absent from both, the tick would evict the connection's byte
	// baseline and the next tick would recount its lifetime bytes against a zero baseline.
	if m.tp != nil {
		m.tp.recordClosed(r.id, r.outbound, up, down)
	}
	m.conns.Delete(r.id)
	m.activeCount.Add(-1)

	if o := m.currentObserver(); o != nil {
		o.OnClose(ConnClose{
			ConnAttrs:       r.attrs(),
			DurationSeconds: time.Since(r.createdAt).Seconds(),
			Uplink:          up,
			Downlink:        down,
		})
	}
}

// Connections satisfies groups.ConnectionManager (github.com/getlantern/lantern-box/adapter/groups),
// returning the active connections as synthesized trafficontrol.TrackerMetadata.
func (m *connTracker) Connections() []trafficontrol.TrackerMetadata {
	var out []trafficontrol.TrackerMetadata
	for _, r := range m.conns.Iter() {
		out = append(out, r.trackerMetadata())
	}
	return out
}

// activeConnections returns the current active connections as IPC Connection values. An empty
// slice is returned if there are no active connections.
func (m *connTracker) activeConnections() []Connection {
	out := make([]Connection, 0)
	for _, r := range m.conns.Iter() {
		out = append(out, newConnection(r))
	}
	return out
}

// activeStats returns the active connection count and the per-(raw)-outbound-tag active counts.
func (m *connTracker) activeStats() (count int, perOutbound map[string]int) {
	perOutbound = make(map[string]int)
	for _, r := range m.conns.Iter() {
		count++
		perOutbound[r.outbound]++
	}
	return count, perOutbound
}

func (m *connTracker) activeConnectionCount() int64 {
	return m.activeCount.Load()
}

func (m *connTracker) closeRecord(r *record) {
	if r == nil || r.closer == nil {
		return
	}
	_ = r.closer.Close()
}

// closeConn closes the connection with id by closing its wrapper, which folds it out via leave.
func (m *connTracker) closeConn(id uuid.UUID) {
	r, ok := m.conns.Load(id)
	if !ok {
		return
	}
	m.closeRecord(r)
}

// closeAllTracked closes every live connection. It is the hard-reclaim path when conntrack is
// compiled out (otherwise conntrack.Close drains every dialed conn).
func (m *connTracker) closeAllTracked() {
	for _, r := range m.conns.Iter() {
		m.closeRecord(r)
	}
}

// tcpConn and udpConn wrap a counted connection. Upstream/ReaderReplaceable/WriterReplaceable let
// bufio unwrap to the underlying conn for its vectorised and read-waiter fast paths. Close first
// closes the wrapped conn, then folds the connection out of the tracker so the final accounting
// snapshot includes any bytes counted by in-flight I/O that completes as close races it.
type tcpConn struct {
	N.ExtendedConn
	rec *record
	ct  *connTracker
}

func (c *tcpConn) Close() error {
	err := c.ExtendedConn.Close()
	c.ct.leave(c.rec)
	return err
}

func (c *tcpConn) Upstream() any           { return c.ExtendedConn }
func (c *tcpConn) ReaderReplaceable() bool { return true }
func (c *tcpConn) WriterReplaceable() bool { return true }

type udpConn struct {
	N.PacketConn
	rec *record
	ct  *connTracker
}

func (c *udpConn) Close() error {
	err := c.PacketConn.Close()
	c.ct.leave(c.rec)
	return err
}

func (c *udpConn) Upstream() any           { return c.PacketConn }
func (c *udpConn) ReaderReplaceable() bool { return true }
func (c *udpConn) WriterReplaceable() bool { return true }

// Compile-time guard that the wrappers implement the interfaces sing-box's router expects.
var (
	_ net.Conn     = (*tcpConn)(nil)
	_ N.PacketConn = (*udpConn)(nil)
)
