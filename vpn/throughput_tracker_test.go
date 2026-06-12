package vpn

import (
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRec(outbound string) *record {
	id, err := uuid.NewV4()
	if err != nil {
		panic(err)
	}
	return &record{id: id, createdAt: time.Now(), outbound: outbound}
}

// addBytes keeps the record's per-conn counters and the tracker's global totals in sync; updating
// only one side produces phantom throughput in the next sample.
func addBytes(ct *connTracker, r *record, up, down int64) {
	r.upload.Add(up)
	r.download.Add(down)
	ct.pushUploaded(up)
	ct.pushDownloaded(down)
}

func TestThroughputTracker_Sample(t *testing.T) {
	tests := []struct {
		name       string
		run        func(ct *connTracker, tr *throughputTracker, t0 time.Time)
		wantPer    map[string]Throughput
		wantGlobal Throughput
	}{
		{
			name: "computes per-outbound and global bps from byte deltas",
			run: func(ct *connTracker, tr *throughputTracker, t0 time.Time) {
				a, b := newRec("vpn-a"), newRec("vpn-b")
				ct.join(a)
				ct.join(b)
				addBytes(ct, a, 125, 250)
				addBytes(ct, b, 500, 1000)
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
			run: func(ct *connTracker, tr *throughputTracker, t0 time.Time) {
				live, closing := newRec("vpn-a"), newRec("vpn-a")
				ct.join(live)
				ct.join(closing)
				addBytes(ct, live, 100, 0)
				addBytes(ct, closing, 400, 0)
				ct.leave(closing)
				tr.sample(t0.Add(time.Second))
			},
			wantPer:    map[string]Throughput{"vpn-a": {Up: 500 * 8}},
			wantGlobal: Throughput{Up: 500 * 8},
		},
		{
			name: "counts a connection that opens and closes within one window",
			run: func(ct *connTracker, tr *throughputTracker, t0 time.Time) {
				c := newRec("vpn-a")
				ct.join(c)
				addBytes(ct, c, 300, 100)
				ct.leave(c)
				tr.sample(t0.Add(time.Second))
			},
			wantPer:    map[string]Throughput{"vpn-a": {Up: 300 * 8, Down: 100 * 8}},
			wantGlobal: Throughput{Up: 300 * 8, Down: 100 * 8},
		},
		{
			name: "non-positive elapsed leaves baselines untouched for the next tick",
			run: func(ct *connTracker, tr *throughputTracker, t0 time.Time) {
				a := newRec("vpn-a")
				ct.join(a)
				addBytes(ct, a, 100, 200)
				tr.sample(t0)

				addBytes(ct, a, 50, 50)
				tr.sample(t0.Add(time.Second))
			},
			wantPer:    map[string]Throughput{"vpn-a": {Up: 150 * 8, Down: 250 * 8}},
			wantGlobal: Throughput{Up: 150 * 8, Down: 250 * 8},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := newConnTracker()
			tr := newThroughputTracker(ct, time.Second)
			ct.tp = tr
			t0 := time.Unix(int64(1000+i), 0)
			tr.lastTickAt = t0
			tt.run(ct, tr, t0)
			assert.Equal(t, tt.wantPer, tr.PerOutbound())
			assert.Equal(t, tt.wantGlobal, tr.Global())
		})
	}
}

func TestThroughputTracker_PerOutboundIsIsolatedCopy(t *testing.T) {
	ct := newConnTracker()
	tr := newThroughputTracker(ct, time.Second)
	ct.tp = tr
	a := newRec("vpn-a")
	ct.join(a)
	addBytes(ct, a, 10, 10)

	t0 := time.Unix(4000, 0)
	tr.lastTickAt = t0
	tr.sample(t0.Add(time.Second))

	snap := tr.PerOutbound()
	require.Equal(t, Throughput{Up: 80, Down: 80}, snap["vpn-a"])
	snap["vpn-a"] = Throughput{Up: 999}
	assert.Equal(t, Throughput{Up: 80, Down: 80}, tr.PerOutbound()["vpn-a"])
}

func TestThroughputTracker_OutboundUnknownTag(t *testing.T) {
	tr := newThroughputTracker(newConnTracker(), time.Second)
	assert.Equal(t, Throughput{}, tr.Outbound("missing"))
}

func TestThroughputTracker_NonPositiveIntervalUsesDefault(t *testing.T) {
	ct := newConnTracker()
	for _, interval := range []time.Duration{0, -time.Second} {
		tr := newThroughputTracker(ct, interval)
		assert.Equal(t, defaultThroughputSampleInterval, tr.interval)
	}
}
