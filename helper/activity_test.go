package helper

import (
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
