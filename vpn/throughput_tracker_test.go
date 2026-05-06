package vpn

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing-box/experimental/clashapi/trafficontrol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeTracker struct {
	md trafficontrol.TrackerMetadata
}

func (f *fakeTracker) Metadata() trafficontrol.TrackerMetadata { return f.md }
func (f *fakeTracker) Close() error                            { return nil }

func newFakeTracker(outbound string) *fakeTracker {
	id, err := uuid.NewV4()
	if err != nil {
		panic(err)
	}
	return &fakeTracker{
		md: trafficontrol.TrackerMetadata{
			ID:        id,
			CreatedAt: time.Now(),
			Upload:    new(atomic.Int64),
			Download:  new(atomic.Int64),
			Outbound:  outbound,
		},
	}
}

// addBytes keeps the fake tracker and manager totals in sync; updating only one side
// produces phantom throughput in the next sample.
func addBytes(mgr *trafficontrol.Manager, t *fakeTracker, up, down int64) {
	t.md.Upload.Add(up)
	t.md.Download.Add(down)
	mgr.PushUploaded(up)
	mgr.PushDownloaded(down)
}

func TestThroughputTracker_Sample(t *testing.T) {
	tests := []struct {
		name       string
		run        func(mgr *trafficontrol.Manager, tr *throughputTracker, t0 time.Time)
		wantPer    map[string]Throughput
		wantGlobal Throughput
	}{
		{
			name: "computes per-outbound and global bps from byte deltas",
			run: func(mgr *trafficontrol.Manager, tr *throughputTracker, t0 time.Time) {
				a, b := newFakeTracker("vpn-a"), newFakeTracker("vpn-b")
				mgr.Join(a)
				mgr.Join(b)
				addBytes(mgr, a, 125, 250)
				addBytes(mgr, b, 500, 1000)
				tr.sample(t0.Add(time.Second))
			},
			wantPer: map[string]Throughput{
				"vpn-a": {Up: 125 * 8, Down: 250 * 8},
				"vpn-b": {Up: 500 * 8, Down: 1000 * 8},
			},
			wantGlobal: Throughput{Up: 625 * 8, Down: 1250 * 8},
		},
		{
			name: "includes bytes from connections closed during the window",
			run: func(mgr *trafficontrol.Manager, tr *throughputTracker, t0 time.Time) {
				live, closing := newFakeTracker("vpn-a"), newFakeTracker("vpn-a")
				mgr.Join(live)
				mgr.Join(closing)
				addBytes(mgr, live, 100, 0)
				addBytes(mgr, closing, 400, 0)
				mgr.Leave(closing)
				tr.sample(t0.Add(time.Second))
			},
			wantPer:    map[string]Throughput{"vpn-a": {Up: 500 * 8}},
			wantGlobal: Throughput{Up: 500 * 8},
		},
		{
			name: "non-positive elapsed leaves baselines untouched for the next tick",
			run: func(mgr *trafficontrol.Manager, tr *throughputTracker, t0 time.Time) {
				a := newFakeTracker("vpn-a")
				mgr.Join(a)
				addBytes(mgr, a, 100, 200)
				tr.sample(t0)

				addBytes(mgr, a, 50, 50)
				tr.sample(t0.Add(time.Second))
			},
			wantPer:    map[string]Throughput{"vpn-a": {Up: 150 * 8, Down: 250 * 8}},
			wantGlobal: Throughput{Up: 150 * 8, Down: 250 * 8},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := trafficontrol.NewManager()
			tr := newThroughputTracker(mgr, time.Second)
			t0 := time.Unix(int64(1000+i), 0)
			tr.lastTickAt = t0
			tt.run(mgr, tr, t0)
			assert.Equal(t, tt.wantPer, tr.PerOutbound())
			assert.Equal(t, tt.wantGlobal, tr.Global())
		})
	}
}

func TestThroughputTracker_PerOutboundIsIsolatedCopy(t *testing.T) {
	mgr := trafficontrol.NewManager()
	tr := newThroughputTracker(mgr, time.Second)
	a := newFakeTracker("vpn-a")
	mgr.Join(a)
	addBytes(mgr, a, 10, 10)

	t0 := time.Unix(4000, 0)
	tr.lastTickAt = t0
	tr.sample(t0.Add(time.Second))

	snap := tr.PerOutbound()
	require.Equal(t, Throughput{Up: 80, Down: 80}, snap["vpn-a"])
	snap["vpn-a"] = Throughput{Up: 999}
	assert.Equal(t, Throughput{Up: 80, Down: 80}, tr.PerOutbound()["vpn-a"])
}

func TestThroughputTracker_OutboundUnknownTag(t *testing.T) {
	tr := newThroughputTracker(trafficontrol.NewManager(), time.Second)
	assert.Equal(t, Throughput{}, tr.Outbound("missing"))
}
