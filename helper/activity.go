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

	// saveMu serializes save, so two saves cannot write the file out of
	// order; it is taken before mu.
	saveMu sync.Mutex

	mu    sync.Mutex
	zone  *time.Location // loc's last answer; Sample refreshes it
	cur   Activity
	saved Activity // what the file holds; save writes only when cur differs
	// lastEpoch and lastBytes are the counters at the last sample, or at
	// construction. The counters are process-wide, so a tracker built for a
	// backend recreated in the same process must not count what an earlier
	// one already did; in a fresh process they are near zero at
	// construction, so bytes still count from process start.
	lastEpoch string
	lastBytes uint64
	// every is Run's interval and lastTick when the last tick or Flush
	// sampled; Flush counts the time since, at most every.
	every    time.Duration
	lastTick time.Time
}

// NewActivityTracker returns a tracker that saves to path and samples read.
func NewActivityTracker(path string, read Sharing) *ActivityTracker {
	t := &ActivityTracker{path: path, read: read, now: time.Now, loc: LocalZone}
	_, t.lastEpoch, t.lastBytes = read()
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

// days returns today and tomorrow in zone.
func (t *ActivityTracker) days(zone *time.Location) (today, tomorrow string) {
	now := t.now().In(zone)
	return now.Format("2006-01-02"), now.AddDate(0, 0, 1).Format("2006-01-02")
}

// isNewDay reports whether today replaces day. Days are YYYY-MM-DD, so they
// compare as strings. A later day replaces it; so does one more than a day
// earlier, which means day was saved while the clock ran ahead. A clock that
// stepped back across midnight, by less than a day, keeps the later day.
func isNewDay(day, today, tomorrow string) bool {
	return today > day || day > tomorrow
}

// cachedZone returns the zone Sample last resolved, resolving it, outside
// t.mu, only if no sample has run. Resolving can walk the zone database, and
// the app polls Today often.
func (t *ActivityTracker) cachedZone() *time.Location {
	t.mu.Lock()
	zone := t.zone
	t.mu.Unlock()
	if zone != nil {
		return zone
	}
	zone = t.loc()
	t.mu.Lock()
	if t.zone == nil {
		t.zone = zone
	}
	t.mu.Unlock()
	return zone
}

// Today returns today's totals.
func (t *ActivityTracker) Today() Activity {
	today, tomorrow := t.days(t.cachedZone())
	t.mu.Lock()
	defer t.mu.Unlock()
	if isNewDay(t.cur.Day, today, tomorrow) {
		return Activity{Day: today}
	}
	return t.cur
}

// Sample adds one interval: elapsed counts as active if sharing runs now, and
// the bytes relayed since the last sample are added unless the counters were
// reset in between. It holds the lock across read, which only copies two
// snapshots, so concurrent samples cannot count the same bytes twice.
func (t *ActivityTracker) Sample(elapsed time.Duration) {
	zone := t.loc() // outside the lock: it can load many zones
	today, tomorrow := t.days(zone)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.zone = zone
	running, epoch, bytes := t.read()
	if isNewDay(t.cur.Day, today, tomorrow) {
		t.cur = Activity{Day: today}
	}
	if running {
		t.cur.ActiveSeconds += int64(elapsed.Round(time.Second) / time.Second)
	}
	if epoch == t.lastEpoch && bytes >= t.lastBytes {
		t.cur.RelayedBytes += bytes - t.lastBytes
	}
	t.lastEpoch, t.lastBytes = epoch, bytes
}

// Run samples every interval until ctx ends, saving after a sample that
// changed something. A device that never shares writes nothing but the
// first sample of each day. It takes no sample when ctx ends: the backend's
// Close calls Flush first, while sharing is still running.
func (t *ActivityTracker) Run(ctx context.Context, every time.Duration) {
	t.mu.Lock()
	t.every, t.lastTick = every, time.Now()
	t.mu.Unlock()
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.Flush()
		}
	}
}

// Flush samples the time since the last tick or Flush, capped at one
// interval because time the device slept is not time it shared, and saves.
// Ticks and flushes each claim the span since the one before, so no interval
// counts twice. Before Run starts it counts bytes only.
func (t *ActivityTracker) Flush() {
	t.mu.Lock()
	now := time.Now()
	var elapsed time.Duration
	if !t.lastTick.IsZero() {
		elapsed = min(now.Sub(t.lastTick), t.every)
	}
	t.lastTick = now
	t.mu.Unlock()
	t.Sample(elapsed)
	t.save()
}

func (t *ActivityTracker) save() {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
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
