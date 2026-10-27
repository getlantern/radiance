package helper

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/settings"
)

type fakeSharing struct {
	running bool
	epoch   string
	bytes   uint64
}

func (f *fakeSharing) read() (bool, string, uint64) { return f.running, f.epoch, f.bytes }

func TestActivityCountsTimeAndBytesWhileSharing(t *testing.T) {
	sh := &fakeSharing{running: true, epoch: "e1"}
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.Local)
	tr := NewActivityTracker(filepath.Join(t.TempDir(), "a.json"), sh.read)
	tr.now = func() time.Time { return now }

	tr.Sample(time.Minute) // primes the byte baseline
	sh.bytes = 1000
	tr.Sample(time.Minute)
	sh.running = false
	sh.bytes = 1500
	tr.Sample(time.Minute)
	got := tr.Today()
	assert.Equal(t, "2026-10-23", got.Day)
	assert.Equal(t, int64(120), got.ActiveSeconds, "only samples taken while sharing count")
	assert.Equal(t, uint64(1500), got.RelayedBytes)

	sh.epoch, sh.bytes = "e2", 10 // a restarted counter
	tr.Sample(time.Minute)
	assert.Equal(t, uint64(1500), tr.Today().RelayedBytes, "a new epoch re-baselines rather than adding")

	now = now.Add(24 * time.Hour)
	assert.Equal(t, Activity{Day: "2026-10-24"}, tr.Today(), "a new day starts at zero")
}

func TestActivitySavesOnlyOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	sh := &fakeSharing{epoch: "e"}
	now := time.Date(2026, 10, 23, 10, 0, 0, 0, time.Local)
	tr := NewActivityTracker(path, sh.read)
	tr.now = func() time.Time { return now }
	tr.Sample(time.Minute)
	tr.save()
	info, err := os.Stat(path)
	require.NoError(t, err, "the first sample of a day starts the file")
	first := info.ModTime()
	require.NoError(t, os.Chtimes(path, first.Add(-time.Hour), first.Add(-time.Hour)))
	for range 5 {
		tr.Sample(time.Minute) // not sharing: nothing changes
		tr.save()
	}
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, first.Add(-time.Hour).Unix(), info.ModTime().Unix(), "idle samples write nothing")
	sh.running = true
	tr.Sample(time.Minute)
	tr.save()
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.NotEqual(t, first.Add(-time.Hour).Unix(), info.ModTime().Unix(), "a change is saved")
}

func TestActivitySurvivesARestartTheSameDay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	sh := &fakeSharing{running: true, epoch: "e"}
	tr := NewActivityTracker(path, sh.read)
	tr.Sample(30 * time.Second)
	tr.save()
	again := NewActivityTracker(path, sh.read)
	assert.Equal(t, int64(30), again.Today().ActiveSeconds)
	require.FileExists(t, path)
}

// The file is on-device state: it holds the day and the two totals, and
// nothing else.
func TestActivityFileHoldsOnlyTheTotals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	sh := &fakeSharing{running: true, epoch: "e"}
	tr := NewActivityTracker(path, sh.read)
	tr.Sample(time.Minute)
	tr.save()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var fields map[string]any
	require.NoError(t, json.Unmarshal(data, &fields))
	assert.ElementsMatch(t, []string{"day", "active_seconds", "relayed_bytes"}, slices.Collect(maps.Keys(fields)))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

// Android and iOS hard-wire time.Local to UTC, so the day follows the
// device's zone as radiance resolves it, not time.Local.
func TestActivityDayFollowsTheDeviceZone(t *testing.T) {
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })
	tehran, err := time.LoadLocation("Asia/Tehran")
	require.NoError(t, err)

	sh := &fakeSharing{running: true, epoch: "e"}
	tr := NewActivityTracker(filepath.Join(t.TempDir(), "a.json"), sh.read)
	tr.loc = func() *time.Location { return tehran }
	tr.now = func() time.Time { return time.Date(2026, 10, 23, 21, 0, 0, 0, time.UTC) } // 00:30 on the 24th in Tehran
	tr.Sample(time.Minute)
	assert.Equal(t, "2026-10-24", tr.Today().Day)
}

func TestLocalZoneUsesTheDeviceZone(t *testing.T) {
	prev := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = prev })
	prevTZ := settings.GetString(settings.TimeZoneKey)
	require.NoError(t, settings.Set(settings.TimeZoneKey, "Asia/Tehran"))
	t.Cleanup(func() { _ = settings.Set(settings.TimeZoneKey, prevTZ) })
	assert.Equal(t, "Asia/Tehran", LocalZone().String())

	assert.Equal(t, time.Local, zoneOrLocal(""), "an unknown zone falls back to time.Local")
	assert.Equal(t, time.Local, zoneOrLocal("Not/AZone"))
}

// A clock that steps back across midnight keeps today's totals rather than
// starting the earlier day over.
func TestActivityIgnoresAClockSteppingBack(t *testing.T) {
	sh := &fakeSharing{running: true, epoch: "e"}
	now := time.Date(2026, 10, 24, 0, 0, 30, 0, time.UTC)
	tr := NewActivityTracker(filepath.Join(t.TempDir(), "a.json"), sh.read)
	tr.loc = func() *time.Location { return time.UTC }
	tr.now = func() time.Time { return now }
	tr.Sample(time.Minute)
	now = now.Add(-time.Minute) // 23:59:30 on the 23rd
	assert.Equal(t, Activity{Day: "2026-10-24", ActiveSeconds: 60}, tr.Today())
	tr.Sample(time.Minute)
	assert.Equal(t, Activity{Day: "2026-10-24", ActiveSeconds: 120}, tr.Today())
}

// The baseline is taken at construction, where a fresh process's counters
// are zero, so bytes count from the first sample on, not from the second.
func TestActivityCountsBytesFromConstruction(t *testing.T) {
	sh := &fakeSharing{running: true, epoch: "e"}
	tr := NewActivityTracker(filepath.Join(t.TempDir(), "a.json"), sh.read)
	sh.bytes = 700
	tr.Sample(time.Minute)
	assert.Equal(t, uint64(700), tr.Today().RelayedBytes)
}

// A backend rebuilt in the same process (the mobile fallback) gets a new
// tracker on the same file while the process-wide counters are already
// non-zero. It must not add them again.
func TestActivitySecondTrackerInAProcessAddsNothingTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	sh := &fakeSharing{running: true, epoch: "e"}
	first := NewActivityTracker(path, sh.read)
	sh.bytes = 500
	first.Sample(time.Minute)
	first.save()

	second := NewActivityTracker(path, sh.read)
	second.Sample(time.Minute)
	assert.Equal(t, uint64(500), second.Today().RelayedBytes, "counters that have not moved add nothing")
	sh.bytes = 800
	second.Sample(time.Minute)
	assert.Equal(t, uint64(800), second.Today().RelayedBytes)
}

// A day saved while the clock ran ahead is dropped once the clock is
// corrected by more than a day, rather than holding until the calendar
// catches up.
func TestActivityDropsADayFarInTheFuture(t *testing.T) {
	sh := &fakeSharing{running: true, epoch: "e"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	tr := NewActivityTracker(filepath.Join(t.TempDir(), "a.json"), sh.read)
	tr.loc = func() *time.Location { return time.UTC }
	tr.now = func() time.Time { return now }
	tr.Sample(time.Minute)
	now = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, Activity{Day: "2026-10-23"}, tr.Today())
	tr.Sample(time.Minute)
	assert.Equal(t, Activity{Day: "2026-10-23", ActiveSeconds: 60}, tr.Today())
}

// Run takes a last sample when it stops, so the minute before a shutdown is
// saved.
func TestActivityRunSamplesBeforeItStops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	sh := &fakeSharing{running: true, epoch: "e"}
	tr := NewActivityTracker(path, sh.read)
	sh.bytes = 42
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tr.Run(ctx, time.Hour); close(done) }()
	cancel()
	<-done
	assert.Equal(t, uint64(42), NewActivityTracker(path, sh.read).Today().RelayedBytes)
}
