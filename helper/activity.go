package helper

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

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

	mu        sync.Mutex
	cur       Activity
	saved     Activity // what the file holds; save writes only when cur differs
	lastEpoch string
	lastBytes uint64
	primed    bool
}

// NewActivityTracker returns a tracker that saves to path and samples read.
func NewActivityTracker(path string, read Sharing) *ActivityTracker {
	t := &ActivityTracker{path: path, read: read, now: time.Now}
	if data, err := os.ReadFile(path); err == nil {
		var a Activity
		if json.Unmarshal(data, &a) == nil {
			t.cur, t.saved = a, a
		}
	}
	return t
}

func dayOf(t time.Time) string { return t.Local().Format("2006-01-02") }

// Today returns today's totals.
func (t *ActivityTracker) Today() Activity {
	t.mu.Lock()
	defer t.mu.Unlock()
	if today := dayOf(t.now()); t.cur.Day != today {
		return Activity{Day: today}
	}
	return t.cur
}

// Sample adds one interval: elapsed counts as active if sharing runs now, and
// the bytes relayed since the last sample are added unless the counters were
// reset in between.
func (t *ActivityTracker) Sample(elapsed time.Duration) {
	running, epoch, bytes := t.read()
	t.mu.Lock()
	defer t.mu.Unlock()
	if today := dayOf(t.now()); t.cur.Day != today {
		t.cur = Activity{Day: today}
	}
	if running {
		t.cur.ActiveSeconds += int64(elapsed / time.Second)
	}
	if t.primed && epoch == t.lastEpoch && bytes >= t.lastBytes {
		t.cur.RelayedBytes += bytes - t.lastBytes
	}
	t.lastEpoch, t.lastBytes, t.primed = epoch, bytes, true
}

// Run samples every interval until ctx ends, saving after a sample that
// changed something. A device that never shares writes nothing but the
// first sample of each day.
func (t *ActivityTracker) Run(ctx context.Context, every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			t.save()
			return
		case <-tk.C:
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
