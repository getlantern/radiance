package backend

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/sagernet/sing/common/memory"
)

// Heap profiling here is an internal diagnostic for the iOS network extension
// memory kill (the OS terminates the extension once its phys_footprint exceeds a
// hard cap). It is not meant for production telemetry.
//
// Each tick, logMemStats writes a heap profile to <DataPath>/pprof/heap-NN.pprof
// and a goroutine profile to goroutine-NN.txt at the same index, rotating across
// maxHeapProfiles files (identify the latest by mtime). The "memory stats" log
// line names both files in its heap_profile and goroutine_profile attrs, alongside
// mem_total (on darwin the phys_footprint the OS kills on), heap_alloc, and
// num_gc, so a profile can be tied to the footprint at that moment; heap_idle
// minus heap_released is memory the runtime holds but is not using (e.g. sync.Pool
// retention).
//
// To analyze, pull the pprof/ dir off-device and run:
//
//	go tool pprof -inuse_space heap-NN.pprof          # what is live now
//	go tool pprof -base heap-00.pprof heap-NN.pprof   # what grew across the window
//
// then top -cum, list <fn>, traces <fn>, or web. Prime suspects for steady growth
// are reachable caches the GC cannot reclaim: fakeip address maps, the DNS cache
// (freelru), urltest history, and the connection-tracking maps.
//
// pprof only sees the Go heap. If mem_total climbs while the profiles stay flat,
// the growth is off-heap (cgo, tun/netstack buffers) and the profile correctly
// shows nothing growing, which is itself the answer.
//
// The goroutine profile is debug=2 text and includes each live goroutine's
// "created by" stack. Use it when a leak of goroutines spawned via `go func()`
// needs to be attributed to its spawn site — the heap profile can't cross that
// `go` boundary. grep the file for the watcher function to count and locate
// instances.

const memStatsInterval = 30 * time.Second

// maxHeapProfiles bounds the rotating set of heap profiles so periodic capture
// can't fill the disk on a long-running client.
const maxHeapProfiles = 20

// StartMemDiagnostics begins periodic memory-stats logging and heap profiling for
// the lifetime of the backend. It is called from the IPC server start so only the
// daemon / system extension profiles, not the in-process backend the mobile app
// spins up as an IPC fallback. See the heap-profiling note above for usage.
func (r *LocalBackend) StartMemDiagnostics() {
	go logMemStats(r.ctx, slog.Default().With("service", "memstats"), memStatsInterval, r.profileDir)
}

// logMemStats logs runtime memory statistics on an interval until ctx is cancelled.
// When profileDir is non-empty it also writes a rotating heap profile on each tick.
func logMemStats(ctx context.Context, logger *slog.Logger, interval time.Duration, profileDir string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var m runtime.MemStats
	var profileIdx int
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runtime.ReadMemStats(&m)
			attrs := []any{
				"heap_alloc", m.HeapAlloc,
				"heap_inuse", m.HeapInuse,
				"heap_idle", m.HeapIdle,
				"heap_released", m.HeapReleased,
				"heap_sys", m.HeapSys,
				"stack_inuse", m.StackInuse,
				"sys", m.Sys,
				"mem_total", memory.Total(),
				"num_goroutine", runtime.NumGoroutine(),
				"num_gc", m.NumGC,
			}
			if profileDir != "" {
				heapPath := filepath.Join(profileDir, fmt.Sprintf("heap-%02d.pprof", profileIdx))
				if err := writeHeapProfile(heapPath); err != nil {
					logger.Warn("failed to write heap profile", "error", err, "path", heapPath)
				} else {
					attrs = append(attrs, "heap_profile", heapPath)
				}
				goroutinePath := filepath.Join(profileDir, fmt.Sprintf("goroutine-%02d.txt", profileIdx))
				if err := writeGoroutineProfile(goroutinePath); err != nil {
					logger.Warn("failed to write goroutine profile", "error", err, "path", goroutinePath)
				} else {
					attrs = append(attrs, "goroutine_profile", goroutinePath)
				}
				profileIdx = (profileIdx + 1) % maxHeapProfiles
			}
			logger.Debug("memory stats", attrs...)
		}
	}
}

// writeHeapProfile forces a GC so the profile reflects reachable memory rather
// than uncollected garbage, then writes the live heap profile to path.
func writeHeapProfile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	runtime.GC()
	return pprof.WriteHeapProfile(f)
}

// writeGoroutineProfile writes the live goroutine profile in debug=2 text format
// so each entry includes the "created by" stack — the spawn-site attribution the
// heap profile loses across the `go` boundary.
func writeGoroutineProfile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pprof.Lookup("goroutine").WriteTo(f, 2)
}

func isProfileFile(filename string) bool {
	return (strings.HasPrefix(filename, "heap-") && strings.HasSuffix(filename, ".pprof")) ||
		strings.HasPrefix(filename, "goroutine-") && strings.HasSuffix(filename, ".txt")
}

func collectProfileAttachments(profileDir string) []string {
	files, err := os.ReadDir(profileDir)
	if err != nil {
		slog.Warn("Failed to read profile directory for issue attachments", "error", err, "dir", profileDir)
		return nil
	}
	var paths []string
	for _, f := range files {
		if isProfileFile(f.Name()) {
			paths = append(paths, filepath.Join(profileDir, f.Name()))
		}
	}
	return paths
}
