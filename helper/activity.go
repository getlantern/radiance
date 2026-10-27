package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/gofrs/flock"

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
//
// More than one tracker can share the file: on mobile, the app can build a
// fallback backend while the extension's keeps running. So a tracker keeps
// only what it has counted and not yet saved (pending), and save adds that to
// the file under a cross-process lock. No tracker writes its own view of the
// totals over another's.
type ActivityTracker struct {
	path string
	read Sharing
	now  func() time.Time
	loc  func() *time.Location

	// saveMu serializes this tracker's saves, and Today against them, so a
	// reader never sees pending both in the file and in memory; it is taken
	// before mu.
	saveMu sync.Mutex

	mu      sync.Mutex
	zone    *time.Location // loc's last answer; Sample refreshes it
	pending Activity       // counted since the last save, for pending.Day
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
	// stopRun ends Run and runDone closes when it has; stopped makes a Run
	// that starts after Stop return at once.
	stopRun context.CancelFunc
	runDone chan struct{}
	stopped bool
}

// NewActivityTracker returns a tracker that saves to path and samples read.
func NewActivityTracker(path string, read Sharing) *ActivityTracker {
	t := &ActivityTracker{path: path, read: read, now: time.Now, loc: LocalZone}
	_, t.lastEpoch, t.lastBytes = read()
	return t
}

// readActivity reads the file, or the zero Activity if there is none. A save
// replaces the file by renaming, so a read sees one whole version.
func readActivity(path string) Activity {
	var a Activity
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &a)
	}
	return a
}

// combine is the day's totals from the file's and the pending ones. Each
// counts only if its day is not stale (see isNewDay); the later of the days
// that count is the day, and only parts for that day add up. With neither,
// it is today with nothing.
func combine(file, pending Activity, today, tomorrow string) Activity {
	fileOK := file.Day != "" && !isNewDay(file.Day, today, tomorrow)
	pendingOK := pending.Day != "" && !isNewDay(pending.Day, today, tomorrow)
	out := Activity{Day: today}
	switch {
	case fileOK && pendingOK:
		out.Day = max(file.Day, pending.Day)
	case fileOK:
		out.Day = file.Day
	case pendingOK:
		out.Day = pending.Day
	default:
		return out
	}
	for _, part := range []Activity{file, pending} {
		if part.Day == out.Day {
			out.ActiveSeconds += part.ActiveSeconds
			out.RelayedBytes += part.RelayedBytes
		}
	}
	return out
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

// Today returns today's totals: the file's, which every tracker on it has
// added to, plus this tracker's pending ones. The file is read without the
// cross-process lock, since a save replaces it whole, by renaming; saveMu
// keeps this tracker's own save from landing between the two reads.
func (t *ActivityTracker) Today() Activity {
	today, tomorrow := t.days(t.cachedZone())
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	file := readActivity(t.path)
	t.mu.Lock()
	pending := t.pending
	t.mu.Unlock()
	return combine(file, pending, today, tomorrow)
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
	if isNewDay(t.pending.Day, today, tomorrow) {
		// Unsaved counts for an earlier day are dropped with that day.
		t.pending = Activity{Day: today}
	}
	if running {
		t.pending.ActiveSeconds += int64(elapsed.Round(time.Second) / time.Second)
	}
	if epoch == t.lastEpoch && bytes >= t.lastBytes {
		t.pending.RelayedBytes += bytes - t.lastBytes
	}
	t.lastEpoch, t.lastBytes = epoch, bytes
}

// Run samples every interval until ctx ends or Stop is called, saving after
// a sample that changed something. A device that never shares writes nothing
// but the first sample of each day. It takes no sample when it ends: the
// backend's Close stops it and then calls Flush, while sharing still runs.
func (t *ActivityTracker) Run(ctx context.Context, every time.Duration) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.every, t.lastTick = every, time.Now()
	t.stopRun, t.runDone = cancel, done
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

// Stop ends Run and waits for it, so no tick saves after Stop returns. A Run
// that starts afterwards returns at once.
func (t *ActivityTracker) Stop() {
	t.mu.Lock()
	t.stopped = true
	stop, done := t.stopRun, t.runDone
	t.mu.Unlock()
	if stop != nil {
		stop()
		<-done
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

// save adds the pending counts to the file. It holds an exclusive lock on a
// sibling lock file across reading the file, rolling its day forward by the
// usual rules, adding, and writing it back, so trackers in other processes
// never lose each other's counts. What it saved is then taken off pending.
// It writes nothing when the file would not change.
func (t *ActivityTracker) save() {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	today, tomorrow := t.days(t.cachedZone())
	t.mu.Lock()
	pending := t.pending
	t.mu.Unlock()
	if pending.Day == "" {
		return
	}
	// Fixed text only: these errors quote the file's path, which can name
	// the device's user.
	lock := flock.New(t.path + ".lock")
	if lock.Lock() != nil {
		slog.Debug("helper: locking activity failed")
		return
	}
	defer func() { _ = lock.Unlock() }()
	file := readActivity(t.path)
	merged := combine(file, pending, today, tomorrow)
	if merged != file {
		data, err := json.Marshal(merged)
		if err != nil {
			return
		}
		// Private: only the backend reads the file; the app asks over IPC.
		if atomicfile.WriteFile(t.path, data, fileperm.Private) != nil {
			slog.Debug("helper: saving activity failed")
			return
		}
	}
	t.mu.Lock()
	if t.pending.Day == pending.Day {
		t.pending.ActiveSeconds -= pending.ActiveSeconds
		t.pending.RelayedBytes -= pending.RelayedBytes
	}
	t.mu.Unlock()
}
