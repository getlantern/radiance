package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"

	"github.com/sagernet/sing/common/buf"
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
//
// The buf_live / buf_gets / buf_puts attrs come from a wrapper around sing's relay
// buffer allocator (see bufPool). buf_live (gets minus puts) is the count of
// buffers currently checked out by active relay loops; comparing buf_live ×
// buffer-size against the heap profile's sing buffer-pool size splits the pool into
// in-use vs idle slack. A widening gets-minus-puts gap signals buffers dropped
// without release (the Leak error paths), not steady-state growth.
//
// buf_limit is the per-size-class retention cap (RADIANCE_BUF_POOL_LIMIT, 0 =
// unbounded). When set, idle buffers above the cap are dropped to GC instead of
// pooled; too low a value trades memory for allocation churn, so size it from a
// buf_live capture rather than guessing.
//
// RADIANCE_BUF_POOL_AUTOTUNE turns that capture into a two-run loop without manual
// analysis: with autotune on, a run records its peak buf_live and a derived cap to
// <DataPath>/bufpool-autotune.json each tick (logged as buf_peak_live / buf_reco),
// and the next run applies that cap on startup unless an explicit limit is set. The
// derived cap is deliberately generous, not optimal — enough to gather data under a
// real limit in one test cycle.

const memStatsInterval = 30 * time.Second

// bufPool wraps sing's buffer allocator for the memory diagnostics. It is installed
// in init, before any tunnel relay runs, because reassigning the allocator once
// goroutines are allocating buffers would race on buf.DefaultAllocator. The
// retention limit arrives later (via env, applied with SetLimit) since on iOS it
// is delivered through Options.EnvOverrides, after package init.
var bufPool *bufAlloc

func init() {
	bufPool = &bufAlloc{inner: buf.DefaultAllocator}
	buf.DefaultAllocator = bufPool
}

// sing's allocator pools power-of-two buffers from 64 B to 64 KB across
// bufPoolClasses size classes; bufAlloc mirrors that geometry when bounding.
const (
	bufPoolMinShift = 6                                           // 1<<6 = 64 B, smallest class
	bufPoolClasses  = 11                                          // 64 B .. 64 KB
	minBufSize      = 1 << bufPoolMinShift                        // 64
	maxBufSize      = 1 << (bufPoolMinShift + bufPoolClasses - 1) // 65536
)

// bufAlloc wraps a buf.Allocator to count outstanding buffers for the memory
// diagnostics and, when SetLimit is given a positive cap, bound retained idle
// buffers per size class — dropping excess to GC to prevent the unbounded pool
// growth that OOM-kills the iOS extension and that GOMEMLIMIT alone does not
// prevent. limit is fixed by SetLimit before relay traffic and read atomically
// thereafter.
type bufAlloc struct {
	inner buf.Allocator
	limit atomic.Int64
	pools [bufPoolClasses]chan []byte

	live atomic.Int64
	gets atomic.Int64
	puts atomic.Int64
}

// SetLimit sets the per-size-class retention cap (0 disables bounding). It must be
// called before any relay traffic: it publishes the free-lists before storing the
// limit, so a concurrent Get/Put still in inner mode never observes a half-built
// pool once the atomic limit turns bounding on.
func (a *bufAlloc) SetLimit(limit int) {
	if limit > 0 {
		for i := range a.pools {
			a.pools[i] = make(chan []byte, limit)
		}
	}
	a.limit.Store(int64(limit))
}

func (a *bufAlloc) Get(size int) []byte {
	a.gets.Add(1)
	a.live.Add(1)
	if a.limit.Load() == 0 {
		return a.inner.Get(size)
	}
	if size <= 0 || size > maxBufSize {
		return nil
	}
	idx := bufClass(size)
	select {
	case b := <-a.pools[idx]:
		return b[:size]
	default:
		return make([]byte, size, 1<<(idx+bufPoolMinShift))
	}
}

// Put counts the release even when the buffer can't be pooled (inner rejection, or
// a full free-list), since the caller has handed it back regardless.
func (a *bufAlloc) Put(b []byte) error {
	a.puts.Add(1)
	a.live.Add(-1)
	if a.limit.Load() == 0 {
		return a.inner.Put(b)
	}
	c := cap(b)
	if c < minBufSize || c > maxBufSize || c != 1<<(bits.Len32(uint32(c))-1) {
		return errors.New("bufAlloc: incorrect buffer size")
	}
	idx := bits.Len32(uint32(c)) - 1 - bufPoolMinShift
	select {
	case a.pools[idx] <- b[:c]:
	default: // free-list full; drop and let GC reclaim
	}
	return nil
}

// bufClass returns the size-class index for size (0 < size <= maxBufSize): the
// smallest power-of-two class >= max(size, 64).
func bufClass(size int) int {
	if size <= minBufSize {
		return 0
	}
	i := bits.Len32(uint32(size)) - 1
	if size != 1<<i {
		i++
	}
	return i - bufPoolMinShift
}

const bufPoolTuningFile = "bufpool-autotune.json"

func bufPoolTuningPath(dataDir string) string {
	return filepath.Join(dataDir, "pprof", bufPoolTuningFile)
}

// bufPoolTuning is the autotune observation carried across restarts: a run records
// its peak checked-out buffer count and a derived cap, so the next run can apply
// the cap (when no explicit limit is set) without manual log analysis.
type bufPoolTuning struct {
	ObservedPeakLive int64 `json:"observed_peak_live"`
	RecommendedLimit int   `json:"recommended_limit"`
}

func readBufPoolTuning(path string) (bufPoolTuning, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return bufPoolTuning{}, false
	}
	var t bufPoolTuning
	if err := json.Unmarshal(data, &t); err != nil {
		return bufPoolTuning{}, false
	}
	return t, true
}

func writeBufPoolTuning(path string, t bufPoolTuning) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0644)
}

// recommendedBufLimit derives a per-size-class cap from the peak global checked-out
// count: peak + 25% headroom, rounded up to a multiple of 64 and floored so a light
// run can't pin the next to a tiny cap. The global peak (sum across classes)
// over-covers any single class on purpose — a deliberately safe, not optimal, first
// value to confirm the mechanism without regressing throughput.
func recommendedBufLimit(peakLive int64) int {
	const minLimit = 256
	v := peakLive + peakLive/4
	v = (v + 63) / 64 * 64
	if v < minLimit {
		v = minLimit
	}
	return int(v)
}

// maxHeapProfiles bounds the rotating set of heap profiles so periodic capture
// can't fill the disk on a long-running client.
const maxHeapProfiles = 20

// StartMemDiagnostics begins periodic memory-stats logging and heap profiling for
// the lifetime of the backend. It is called from the IPC server start so only the
// daemon / system extension profiles, not the in-process backend the mobile app
// spins up as an IPC fallback. See the heap-profiling note above for usage.
func (r *LocalBackend) StartMemDiagnostics() {
	var autotunePath string
	if env.GetBool(env.BufPoolAutotune) {
		autotunePath = bufPoolTuningPath(settings.GetString(settings.DataPathKey))
	}
	go logMemStats(r.ctx, slog.Default().With("service", "memstats"), memStatsInterval, r.profileDir, r.vpnClient.LiveConnectionCount, autotunePath)
}

// logMemStats logs runtime memory statistics on an interval until ctx is cancelled.
// When profileDir is non-empty it also writes a rotating heap profile on each tick.
// connCount reports the active tracked-connection count, with ok false when the
// tunnel is down. When autotunePath is non-empty it records the peak checked-out
// buffer count and a recommended cap there each tick, for the next run to apply.
func logMemStats(ctx context.Context, logger *slog.Logger, interval time.Duration, profileDir string, connCount func() (int, bool), autotunePath string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var m runtime.MemStats
	var profileIdx int
	var peakLive int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runtime.ReadMemStats(&m)
			bufLive := bufPool.live.Load()
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
				"buf_live", bufLive,
				"buf_gets", bufPool.gets.Load(),
				"buf_puts", bufPool.puts.Load(),
				"buf_limit", bufPool.limit.Load(),
			}
			if flows, ok := connCount(); ok {
				attrs = append(attrs, "flows", flows)
			}
			if autotunePath != "" {
				if bufLive > peakLive {
					peakLive = bufLive
				}
				reco := recommendedBufLimit(peakLive)
				if err := writeBufPoolTuning(autotunePath, bufPoolTuning{ObservedPeakLive: peakLive, RecommendedLimit: reco}); err != nil {
					logger.Warn("failed to write buf pool tuning", "error", err, "path", autotunePath)
				}
				attrs = append(attrs, "buf_peak_live", peakLive, "buf_reco", reco)
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
		(strings.HasPrefix(filename, "goroutine-") && strings.HasSuffix(filename, ".txt")) ||
		filename == bufPoolTuningFile
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
