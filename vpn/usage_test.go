package vpn

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	A "github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	sblog "github.com/sagernet/sing-box/log"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
)

func TestUsageClassify(t *testing.T) {
	ct := newConnTracker()
	ct.isPrivate = func(tag string) bool { return tag == "mine" }
	tests := []struct {
		tag, typ string
		want     usageCategory
	}{
		{"direct", C.TypeDirect, usageDirect},
		{"block", C.TypeBlock, usageNone},
		{"dns-out", C.TypeDNS, usageNone},
		{"unresolved", "", usageNone},
		{ProxylessTag, lbC.TypeOutline, usageProxyless},
		{testHelperTag, "samizdat", usagePersonal},
		{testHelperTag + "-r", "samizdat", usagePersonal},
		{"mine", "shadowsocks", usagePrivate},
		{"lantern-srv", "shadowsocks", usageLantern},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, ct.classify(tt.tag, tt.typ), "%s/%s", tt.typ, tt.tag)
	}
	ct.isPrivate = nil
	assert.Equal(t, usageLantern, ct.classify("mine", "shadowsocks"), "without a classifier, servers are Lantern's")
}

// usageConn is a connection that reads and writes any amount at once.
type usageConn struct{ net.Conn }

func (usageConn) Read(p []byte) (int, error)  { return len(p), nil }
func (usageConn) Write(p []byte) (int, error) { return len(p), nil }
func (usageConn) Close() error                { return nil }

// newUsageTracker returns a tracker routing through the proxyless hedge to
// auto's srv-a, counting into a store at a temporary path.
func newUsageTracker(t *testing.T) (*connTracker, *UsageStats, A.Outbound) {
	ct := newConnTracker()
	hedge := namedOutbound{tag: ProxylessHedgeTag, typ: lbC.TypeHedge}
	ct.outbound = stubOutboundManager{outbounds: map[string]A.Outbound{
		ProxylessHedgeTag: hedge,
		AutoSelectTag:     namedGroup{tag: AutoSelectTag, now: "srv-a"},
		"srv-a":           namedOutbound{tag: "srv-a", typ: "shadowsocks"},
		"direct":          namedOutbound{tag: "direct", typ: C.TypeDirect},
	}}
	ct.usage = NewUsageStats(filepath.Join(t.TempDir(), "usage-stats.json"))
	return ct, ct.usage, hedge
}

func routeTCP(t *testing.T, ct *connTracker, ctx context.Context, out A.Outbound) *tcpConn {
	c, ok := ct.RoutedConnection(ctx, usageConn{}, A.InboundContext{}, nil, out).(*tcpConn)
	require.True(t, ok)
	return c
}

// transfer moves up bytes from the device and down bytes to it. The routed
// conn faces the device, so what is read from it goes up.
func transfer(t *testing.T, c net.Conn, up, down int) {
	_, err := c.Read(make([]byte, up))
	require.NoError(t, err)
	_, err = c.Write(make([]byte, down))
	require.NoError(t, err)
}

func TestUsageCountsByPath(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	direct, _ := ct.outbound.Outbound("direct")

	transfer(t, routeTCP(t, ct, context.Background(), direct), 10, 20)
	transfer(t, routeTCP(t, ct, context.Background(), hedge), 30, 40)
	udp, ok := ct.RoutedPacketConnection(context.Background(), nil, A.InboundContext{}, nil, direct).(*udpConn)
	require.True(t, ok)
	ct.countUpload(udp.rec, 5)

	got := stats.peek()
	assert.Equal(t, UsageBytes{Up: 15, Down: 20}, got.Direct)
	assert.Equal(t, UsageBytes{Up: 30, Down: 40}, got.Lantern)
	assert.Zero(t, got.Proxyless)
}

func TestUsageHedgeCommitFlipsToProxyless(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	ctx := sblog.ContextWithNewID(context.Background())

	c := routeTCP(t, ct, ctx, hedge)
	transfer(t, c, 1, 2)
	ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	transfer(t, c, 100, 200)

	got := stats.peek()
	assert.Zero(t, got.Lantern, "bytes before the commit went through First too")
	assert.Equal(t, UsageBytes{Up: 101, Down: 202}, got.Proxyless)

	require.NoError(t, c.Close())
	_, indexed := ct.hedged.Load(c.rec.logID)
	assert.False(t, indexed, "leave drops the index")
}

func TestUsageHedgedBytesWaitForTheReport(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	ctx := sblog.ContextWithNewID(context.Background())

	c := routeTCP(t, ct, ctx, hedge)
	transfer(t, c, 5, 6)
	stats.Flush()
	assert.Zero(t, readUsage(stats.path).Lifetime, "undecided bytes aren't saved under any path")
	ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	stats.Flush()

	file := readUsage(stats.path)
	assert.Zero(t, file.Lifetime.Lantern)
	assert.Equal(t, UsageBytes{Up: 5, Down: 6}, file.Lifetime.Proxyless)
	assert.Equal(t, UsageBytes{Up: 5, Down: 6}, file.ThisMonth.Proxyless)
}

func TestUsageUnreportedHedgedBytesGoToTheFallback(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	c := routeTCP(t, ct, sblog.ContextWithNewID(context.Background()), hedge)
	transfer(t, c, 5, 6)
	assert.Zero(t, stats.peek())

	c.rec.createdAt = time.Now().Add(-usageHedgeWait)
	transfer(t, c, 1, 1)
	assert.Equal(t, UsageBytes{Up: 6, Down: 7}, stats.peek().Lantern, "past the wait, held bytes and new ones count under the fallback")
}

func TestUsageCloseSettlesHeldBytes(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	c := routeTCP(t, ct, sblog.ContextWithNewID(context.Background()), hedge)
	transfer(t, c, 5, 6)
	require.NoError(t, c.Close())
	assert.Equal(t, UsageBytes{Up: 5, Down: 6}, stats.peek().Lantern)
}

func TestUsageCommitRacingCountsLosesNothing(t *testing.T) {
	for range 50 {
		ct, stats, hedge := newUsageTracker(t)
		ctx := sblog.ContextWithNewID(context.Background())
		c := routeTCP(t, ct, ctx, hedge)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 200 {
					ct.countUpload(c.rec, 1)
				}
			}()
		}
		ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
		wg.Wait()
		require.NoError(t, c.Close())
		got := stats.peek()
		require.Equal(t, int64(800), got.Proxyless.Up+got.Lantern.Up, "every byte counted exactly once")
		require.Zero(t, got.Lantern.Up, "the report came before the wait ran out, so every byte is proxyless")
	}
}

func TestUsageIgnoresOtherReports(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	ctx := sblog.ContextWithNewID(context.Background())
	c := routeTCP(t, ct, ctx, hedge)

	ct.FirstCommitted(ctx, "outer-hedge", ProxylessHedgeTag)
	ct.FirstCommitted(ctx, "other-hedge", ProxylessTag)
	ct.FirstCommitted(ctx, ProxylessHedgeTag, "other")
	ct.FirstCommitted(context.Background(), ProxylessHedgeTag, ProxylessTag)
	ct.FirstCommitted(sblog.ContextWithNewID(context.Background()), ProxylessHedgeTag, ProxylessTag)
	transfer(t, c, 3, 4)
	require.NoError(t, c.Close()) // settles the held bytes
	assert.Equal(t, UsageBytes{Up: 3, Down: 4}, stats.peek().Lantern)
	assert.Zero(t, stats.peek().Proxyless)
}

func TestUsageReportAfterLeaveIgnored(t *testing.T) {
	ct, _, hedge := newUsageTracker(t)
	ctx := sblog.ContextWithNewID(context.Background())
	c := routeTCP(t, ct, ctx, hedge)
	require.NoError(t, c.Close())

	ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	assert.Equal(t, usageLantern, usageCategory(c.rec.category.Load()))
}

func TestUsageOnlyHedgedConnectionsIndexed(t *testing.T) {
	ct, _, _ := newUsageTracker(t)
	direct, _ := ct.outbound.Outbound("direct")
	ctx := sblog.ContextWithNewID(context.Background())
	c := routeTCP(t, ct, ctx, direct)
	assert.False(t, c.rec.hedged)
	ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	assert.Equal(t, usageDirect, usageCategory(c.rec.category.Load()))
}

func TestUsageLogIDCollisionNotIndexed(t *testing.T) {
	ct, _, hedge := newUsageTracker(t)
	ctx := sblog.ContextWithNewID(context.Background())
	first := routeTCP(t, ct, ctx, hedge)
	second := routeTCP(t, ct, ctx, hedge)
	require.True(t, first.rec.hedged)
	require.False(t, second.rec.hedged, "an ID already taken isn't indexed again")

	ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	assert.Equal(t, usageProxyless, usageCategory(first.rec.category.Load()))
	assert.Equal(t, usageLantern, usageCategory(second.rec.category.Load()))

	require.NoError(t, second.Close())
	got, ok := ct.hedged.Load(first.rec.logID)
	require.True(t, ok, "the newcomer's leave keeps the first's index")
	assert.Same(t, first.rec, got)
}

func TestHedgeReportsDroppedUntilTrackerSet(t *testing.T) {
	ct, _, hedge := newUsageTracker(t)
	var h hedgeReports
	ctx := sblog.ContextWithNewID(context.Background())
	c := routeTCP(t, ct, ctx, hedge)

	h.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	assert.Equal(t, usageLantern, usageCategory(c.rec.category.Load()))
	h.tracker.Store(ct)
	h.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag)
	assert.Equal(t, usageProxyless, usageCategory(c.rec.category.Load()))
}

// newTestUsageStats returns a store at path whose clock reads *now, in UTC.
func newTestUsageStats(path string, now *time.Time) *UsageStats {
	s := NewUsageStats(path)
	s.now = func() time.Time { return *now }
	s.loc = func() *time.Location { return time.UTC }
	return s
}

func TestUsageStatsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.count(usageProxyless, true, 7)
	s.count(usagePrivate, false, 9)
	s.count(usageNone, true, 1000)
	s.Flush()
	assert.Zero(t, s.peek(), "a save takes the pending totals")

	want := UsageTotals{Proxyless: UsageBytes{Up: 7}, Private: UsageBytes{Down: 9}}
	u := newTestUsageStats(path, &now).Usage()
	assert.True(t, u.Since.Equal(now))
	assert.Equal(t, "2026-11", u.Month)
	assert.Equal(t, want, u.Lifetime)
	assert.Equal(t, want, u.ThisMonth)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.ElementsMatch(t, []string{"since", "month", "lifetime", "this_month"}, keys(raw))
	assert.ElementsMatch(t, []string{"proxyless", "personal", "private", "lantern", "direct"}, keys(raw["lifetime"].(map[string]any)))
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUsageStatsIdleFlushLeavesFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	saved := []byte(`{ "since": "2026-01-01T00:00:00Z", "month": "2026-11" }`)
	require.NoError(t, os.WriteFile(path, saved, 0o600))
	newTestUsageStats(path, &now).Flush()
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, saved, got, "nothing to add, so nothing written")
}

func TestUsageStatsAddsAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a, b := newTestUsageStats(path, &now), newTestUsageStats(path, &now)
	a.count(usageLantern, true, 1)
	b.count(usageLantern, true, 2)
	a.Flush()
	b.Flush()
	b.count(usageLantern, true, 4)
	assert.Equal(t, int64(7), b.Usage().Lifetime.Lantern.Up, "the file's totals plus pending")
	assert.Equal(t, int64(3), a.Usage().Lifetime.Lantern.Up)
}

func TestUsageStatsMonthRollover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.count(usageDirect, false, 10)
	s.Flush()

	now = time.Date(2026, 12, 1, 1, 0, 0, 0, time.UTC)
	u := s.Usage()
	assert.Equal(t, "2026-12", u.Month, "a read rolls the month")
	assert.Zero(t, u.ThisMonth)
	assert.Equal(t, int64(10), u.Lifetime.Direct.Down)

	s.count(usageDirect, false, 5)
	s.Flush()
	file := readUsage(path)
	assert.Equal(t, "2026-12", file.Month, "a save rolls the month")
	assert.Equal(t, int64(5), file.ThisMonth.Direct.Down)
	assert.Equal(t, int64(15), file.Lifetime.Direct.Down)

	now = time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC)
	s.count(usageDirect, false, 1)
	s.Flush()
	assert.Equal(t, "2026-12", readUsage(path).Month, "a clock stepped back keeps the later month")
	assert.Equal(t, int64(6), readUsage(path).ThisMonth.Direct.Down)

	now = time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "2026-10", s.Usage().Month, "a read rolls back a month saved while the clock ran ahead")
	s.count(usageDirect, false, 2)
	s.Flush()
	file = readUsage(path)
	assert.Equal(t, "2026-10", file.Month, "a month saved while the clock ran ahead gives way")
	assert.Equal(t, int64(2), file.ThisMonth.Direct.Down)
	assert.Equal(t, int64(18), file.Lifetime.Direct.Down)
}

func TestUsageStatsReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.count(usagePersonal, true, 10)
	s.Flush()
	s.count(usagePersonal, true, 3)

	now = now.Add(time.Hour)
	require.NoError(t, s.Reset())
	u := s.Usage()
	assert.True(t, u.Since.Equal(now))
	assert.Zero(t, u.Lifetime)
	assert.Zero(t, u.ThisMonth)
	assert.Zero(t, readUsage(path).Lifetime)
}

func TestUsageStatsResetByAnotherStoreDropsPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a, b := newTestUsageStats(path, &now), newTestUsageStats(path, &now)
	a.count(usageLantern, true, 1)
	b.count(usageLantern, true, 2)
	a.Flush()
	b.Flush()
	b.count(usageLantern, true, 40) // counted before a's reset

	now = now.Add(time.Hour)
	require.NoError(t, a.Reset())
	assert.Zero(t, b.Usage().Lifetime, "b's pending bytes predate the reset")
	b.Flush()
	assert.Zero(t, readUsage(path).Lifetime, "b's save must not bring them back")

	b.count(usageLantern, true, 7)
	b.Flush()
	assert.Equal(t, int64(7), readUsage(path).Lifetime.Lantern.Up, "b counts again after the reset")
}

func TestUsageStatsResetKeepsPendingWhenWriteFails(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	require.NoError(t, os.Mkdir(path, 0o700))
	s := newTestUsageStats(path, &now)
	s.count(usageLantern, false, 8)
	require.Error(t, s.Reset())
	assert.Equal(t, int64(8), s.peek().Lantern.Down, "a failed reset loses nothing")
}

func TestUsageStatsKeepsPendingWhenSaveFails(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	// A directory where the file should be: the lock is taken, the write fails.
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	require.NoError(t, os.Mkdir(path, 0o700))
	s := newTestUsageStats(path, &now)
	s.count(usageLantern, false, 8)
	s.Flush()
	assert.Equal(t, int64(8), s.peek().Lantern.Down)
}

func TestUsageStatsStop(t *testing.T) {
	s := NewUsageStats(filepath.Join(t.TempDir(), "usage-stats.json"))
	done := make(chan struct{})
	go func() {
		s.Run(context.Background(), time.Millisecond)
		close(done)
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(s.path)
		return err == nil
	}, time.Second, time.Millisecond, "Run saves")
	s.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop didn't end Run")
	}
}

func TestTunnelWiresUsageAndHedgeReports(t *testing.T) {
	options := O.Options{
		Log:       &O.LogOptions{Disabled: true},
		Outbounds: []O.Outbound{{Type: C.TypeDirect, Tag: "direct", Options: &O.DirectOutboundOptions{}}},
		Experimental: &O.ExperimentalOptions{
			CacheFile: &O.CacheFileOptions{Enabled: true, Path: filepath.Join(t.TempDir(), "cache.db")},
		},
	}
	stats := NewUsageStats(filepath.Join(t.TempDir(), "usage-stats.json"))
	tun := &tunnel{
		dataPath:    t.TempDir(),
		initialMode: AutoSelectTag,
		usage:       &usageSink{stats: stats, isPrivate: func(tag string) bool { return tag == "mine" }},
	}
	t.Cleanup(func() { assert.NoError(t, tun.close()) })
	require.NoError(t, tun.init(t.Context(), options, nil))

	assert.Same(t, &tun.hedgeReports, service.FromContext[lbA.HedgeObserver](tun.ctx), "hedges find the observer")
	assert.Same(t, tun.connTracker, tun.hedgeReports.tracker.Load())
	assert.Same(t, stats, tun.connTracker.usage)
	assert.Equal(t, usagePrivate, tun.connTracker.classify("mine", "shadowsocks"))
}

func TestVPNClientGivesTunnelsUsage(t *testing.T) {
	c := NewVPNClient(t.TempDir(), nil, nil)
	stats := NewUsageStats(filepath.Join(t.TempDir(), "usage-stats.json"))
	c.SetUsageStats(stats, func(string) bool { return true })
	tun := c.newTunnel(BoxOptions{}, AutoSelectTag)
	require.NotNil(t, tun.usage)
	assert.Same(t, stats, tun.usage.stats)
	assert.True(t, tun.usage.isPrivate("any"))
}

func TestUsageStatsIdleSaveSkipsTheDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.Flush()
	require.FileExists(t, path, "the first save pins since")
	require.NoError(t, os.Remove(path))
	s.Flush()
	assert.NoFileExists(t, path, "an idle save doesn't read or write the file")
	s.count(usageDirect, true, 1)
	s.Flush()
	assert.FileExists(t, path)
}

// The per-chunk cost on the data path, with and without usage counting, and
// while a hedged connection's bytes are held.
func BenchmarkCountUpload(b *testing.B) {
	for _, bc := range []struct {
		name   string
		usage  bool
		hedged bool
	}{{"no-usage", false, false}, {"usage", true, false}, {"usage-held", true, true}} {
		b.Run(bc.name, func(b *testing.B) {
			ct := newConnTracker()
			if bc.usage {
				ct.usage = NewUsageStats(filepath.Join(b.TempDir(), "usage-stats.json"))
			}
			r := &record{createdAt: time.Now()}
			r.category.Store(uint32(usageLantern))
			r.undecided.Store(bc.hedged)
			b.ReportAllocs()
			for b.Loop() {
				ct.countUpload(r, 1500)
			}
		})
	}
}

// A settle between a chunk's undecided check and its hold has already
// released; the chunk must release itself rather than wait forever.
func TestUsageChunkHeldAfterSettleIsReleased(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	ctx := sblog.ContextWithNewID(context.Background())
	c := routeTCP(t, ct, ctx, hedge)
	beforeHoldHook = func(*record) { ct.FirstCommitted(ctx, ProxylessHedgeTag, ProxylessTag) }
	t.Cleanup(func() { beforeHoldHook = nil })

	ct.countUpload(c.rec, 9)
	beforeHoldHook = nil
	assert.Equal(t, UsageBytes{Up: 9}, stats.peek().Proxyless)
	assert.Zero(t, c.rec.heldUp.Load())
}

func TestUsageQuietHedgedConnectionSettlesOnTheSamplerTick(t *testing.T) {
	ct, stats, hedge := newUsageTracker(t)
	c := routeTCP(t, ct, sblog.ContextWithNewID(context.Background()), hedge)
	transfer(t, c, 5, 6)
	tp := newThroughputTracker(ct, time.Hour)
	tp.lastTickAt = time.Now()

	tp.sample(time.Now().Add(time.Second))
	assert.Zero(t, stats.peek(), "within the wait the bytes are still held")
	tp.sample(c.rec.createdAt.Add(usageHedgeWait))
	assert.Equal(t, UsageBytes{Up: 5, Down: 6}, stats.peek().Lantern, "the tick settles a quiet connection")
}

func TestUsageStatsKeepsBytesCountedAfterAnotherStoresReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a, b := newTestUsageStats(path, &now), newTestUsageStats(path, &now)
	b.count(usageLantern, true, 2)
	b.Flush()
	a.Flush()

	now = now.Add(time.Hour)
	require.NoError(t, a.Reset())
	b.Flush() // idle: b doesn't see the reset yet

	now = now.Add(5 * time.Hour)
	b.count(usageLantern, true, 9)
	assert.Equal(t, int64(9), b.Usage().Lifetime.Lantern.Up, "counted after the reset, so it shows")
	b.Flush()
	assert.Equal(t, int64(9), readUsage(path).Lifetime.Lantern.Up, "and it is saved, not dropped")
}

// A chunk counted while a save drained can leave a start time with no bytes
// behind it; an idle save must clear it before a later reset makes it stale.
func TestUsageStatsIdleSaveClearsAnOrphanedStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a, b := newTestUsageStats(path, &now), newTestUsageStats(path, &now)
	b.Flush()
	a.Flush()
	b.pendingFrom.Store(now.UnixNano()) // as a count racing take leaves it

	now = now.Add(time.Minute)
	b.Flush() // idle
	now = now.Add(time.Hour)
	require.NoError(t, a.Reset())
	now = now.Add(time.Hour)
	b.count(usageLantern, true, 9)
	b.Flush()
	assert.Equal(t, int64(9), readUsage(path).Lifetime.Lantern.Up, "post-reset bytes don't inherit the orphaned start")
}

// Log IDs are a random number and a creation time; two connections sharing
// only the number are told apart.
func TestUsageLogIDsSharingANumberAreDistinct(t *testing.T) {
	ct, _, hedge := newUsageTracker(t)
	at := time.Now()
	ctxA := sblog.ContextWithID(context.Background(), sblog.ID{ID: 7, CreatedAt: at})
	ctxB := sblog.ContextWithID(context.Background(), sblog.ID{ID: 7, CreatedAt: at.Add(time.Millisecond)})
	a, b := routeTCP(t, ct, ctxA, hedge), routeTCP(t, ct, ctxB, hedge)
	require.True(t, a.rec.hedged)
	require.True(t, b.rec.hedged)

	ct.FirstCommitted(ctxB, ProxylessHedgeTag, ProxylessTag)
	assert.Equal(t, usageLantern, usageCategory(a.rec.category.Load()), "b's report must not reach a")
	assert.Equal(t, usageProxyless, usageCategory(b.rec.category.Load()))
}

func TestUsageStatsSeesAResetBeforeItsFirstSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a := newTestUsageStats(path, &now)
	a.count(usageLantern, true, 1)
	a.Flush()

	b := newTestUsageStats(path, &now) // reads the file's since as it starts
	b.count(usageLantern, true, 40)
	now = now.Add(time.Hour)
	require.NoError(t, a.Reset())
	b.Flush()
	assert.Zero(t, readUsage(path).Lifetime, "b's first save must not restore bytes from before the reset")
}

func TestUsageStatsReadResolvesTheZone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.Flush()
	require.Equal(t, "2026-11", s.Usage().Month)

	s.loc = func() *time.Location { return time.FixedZone("UTC+1", 3600) }
	s.Flush() // idle: doesn't resolve the zone
	assert.Equal(t, "2026-12", s.Usage().Month, "a read uses the device's current zone")
}

func TestUsageStatsDoesNotOverwriteAFileItCantRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-stats.json")
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := newTestUsageStats(path, &now)
	s.count(usageLantern, true, 100)
	s.Flush()

	require.NoError(t, os.Chmod(path, 0))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	s.count(usageLantern, true, 5)
	s.Flush()
	require.NoError(t, os.Chmod(path, 0o600))
	assert.Equal(t, int64(100), readUsage(path).Lifetime.Lantern.Up, "the saved totals survive")
	assert.Equal(t, int64(5), s.peek().Lantern.Up, "and the new bytes wait for the next save")
}
