package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
)

// Activity is today's sharing on this device: how long it ran and how much it
// relayed, on the device's local calendar. It counts paired and pool traffic
// together, as Advanced stats does, and says nothing about whom it helped.
type Activity struct {
	Day           string `json:"day"`
	ActiveSeconds int64  `json:"active_seconds"`
	RelayedBytes  uint64 `json:"relayed_bytes"`
}

// Sharing reads what sharing is doing now: whether either mode runs, an
// identifier for its counters' current epochs, and their cumulative bytes.
type Sharing func() (running bool, epoch string, bytes uint64)

// ActivityTracker builds today's Activity from periodic samples and keeps it
// in a file, so a restart on the same day carries on. The file holds only
// the Activity: no pairing, peer or count of either.
type ActivityTracker struct {
	path string
	read Sharing
	now  func() time.Time
	loc  func() *time.Location

	mu        sync.Mutex
	zone      *time.Location // loc's last answer; Sample refreshes it
	cur       Activity
	saved     Activity // what the file holds; save writes only when cur differs
	lastEpoch string
	// lastBytes starts at 0 because both counters start at 0 with the
	// process; the first sample adopts its epoch rather than re-baselining.
	lastBytes uint64
	epochSeen bool
}

// NewActivityTracker returns a tracker that saves to path and samples read.
func NewActivityTracker(path string, read Sharing) *ActivityTracker {
	t := &ActivityTracker{path: path, read: read, now: time.Now, loc: LocalZone}
	if data, err := os.ReadFile(path); err == nil {
		var a Activity
		if json.Unmarshal(data, &a) == nil {
			t.cur, t.saved = a, a
		}
	}
	return t
}

// LocalZone is the device's time zone. Go hard-wires time.Local to UTC on
// Android and iOS, so the zone comes from common.LocalTimeZone, which asks
// the host app; time.Local is the fallback when that is unknown.
func LocalZone() *time.Location {
	return zoneOrLocal(common.LocalTimeZone())
}

func zoneOrLocal(name string) *time.Location {
	if name == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	return loc
}

// today returns the current day in the device's zone. t.mu is held. The
// zone is resolved once per sample, not per read, since resolving it can
// walk the zone database and the app polls Today often.
func (t *ActivityTracker) today(refresh bool) string {
	if refresh || t.zone == nil {
		t.zone = t.loc()
	}
	return t.now().In(t.zone).Format("2006-01-02")
}

// Today returns today's totals. Days are YYYY-MM-DD, so they compare as
// strings; a clock that stepped back across midnight keeps the later day.
func (t *ActivityTracker) Today() Activity {
	t.mu.Lock()
	defer t.mu.Unlock()
	if today := t.today(false); today > t.cur.Day {
		return Activity{Day: today}
	}
	return t.cur
}

// Sample adds one interval: elapsed counts as active if sharing runs now, and
// the bytes relayed since the last sample are added unless the counters were
// reset in between. It holds the lock across read, which only copies two
// snapshots, so concurrent samples cannot count the same bytes twice.
func (t *ActivityTracker) Sample(elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	running, epoch, bytes := t.read()
	if today := t.today(true); today > t.cur.Day {
		t.cur = Activity{Day: today}
	}
	if running {
		t.cur.ActiveSeconds += int64(elapsed / time.Second)
	}
	if (!t.epochSeen || epoch == t.lastEpoch) && bytes >= t.lastBytes {
		t.cur.RelayedBytes += bytes - t.lastBytes
	}
	t.lastEpoch, t.lastBytes, t.epochSeen = epoch, bytes, true
}

// Run samples every interval until ctx ends, saving after a sample that
// changed something, and takes a last sample when ctx ends. A device that
// never shares writes nothing but the first sample of each day.
func (t *ActivityTracker) Run(ctx context.Context, every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			// Capped at one interval, like a tick: time the device slept
			// is not time it shared.
			t.Sample(min(time.Since(last), every))
			t.save()
			return
		case last = <-tk.C:
			t.Sample(every)
			t.save()
		}
	}
}

func (t *ActivityTracker) save() {
	t.mu.Lock()
	cur := t.cur
	unchanged := cur == t.saved
	t.mu.Unlock()
	if unchanged {
		return
	}
	data, err := json.Marshal(cur)
	if err != nil {
		return
	}
	// Fixed text only: the error quotes the file's path, which can name the
	// device's user.
	// Private: only the backend that writes the file reads it; the app asks
	// over IPC.
	if atomicfile.WriteFile(t.path, data, fileperm.Private) != nil {
		slog.Debug("helper: saving activity failed")
		return
	}
	t.mu.Lock()
	t.saved = cur
	t.mu.Unlock()
}
