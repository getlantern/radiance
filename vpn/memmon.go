package vpn

import (
	"context"
	"io"
	"log/slog"
	runtimeDebug "runtime/debug"
	"slices"

	"github.com/gofrs/uuid/v5"
	"github.com/sagernet/sing-box/common/conntrack"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/vpn/memmon"
)

const (
	// iOS uses dynamic headroom when available and falls back to this budget otherwise.
	defaultIOSMemLimitBytes    = 48 * 1024 * 1024  // 48 MB
	defaultNonIOSMemLimitBytes = 512 * 1024 * 1024 // 512 MB

	// minMemLimitMB is the minimum override. It is floored at GOMEMLIMIT so GC
	// can react before the fixed-budget monitor starts shedding work.
	minMemLimitMB = mobileMemoryLimit / (1024 * 1024)
	maxMemLimitMB = 512
)

// memoryReclaimer implements memmon.Reclaimer. Soft eviction acts on the connection tracker
// (oldest-first); the hard path prefers conntrack.Close, which drains every dialed conn under one
// lock, and falls back to the tracker when conntrack is compiled out (without the with_conntrack
// build tag conntrack.Close is a no-op) so a hard reclaim is never silently lost.
type memoryReclaimer struct {
	tracker *connTracker
}

func (r *memoryReclaimer) ConnectionsOldestFirst() []memmon.ConnectionRef {
	metadata := r.tracker.Connections()
	refs := make([]memmon.ConnectionRef, len(metadata))
	for i, conn := range metadata {
		refs[i] = memmon.ConnectionRef{ID: conn.ID, CreatedAt: conn.CreatedAt}
	}
	slices.SortFunc(refs, func(a, b memmon.ConnectionRef) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return refs
}

func (r *memoryReclaimer) FreeOSMemory() {
	runtimeDebug.FreeOSMemory()
}

func (r *memoryReclaimer) OpenConnectionCount() int {
	return int(r.tracker.activeConnectionCount())
}

func (r *memoryReclaimer) TotalDialedConnections() int {
	if conntrack.Enabled {
		return conntrack.Count()
	}
	return int(r.tracker.activeConnectionCount())
}

func (r *memoryReclaimer) CloseConn(id uuid.UUID) {
	r.tracker.closeConn(id)
}

func (r *memoryReclaimer) CloseAllConnections() {
	closeAllRouted(r.tracker)
}

// Prefer conntrack.Close when available, and fall back to tracked connections
// when conntrack support is compiled out.
func closeAllRouted(tracker *connTracker) {
	if conntrack.Enabled {
		conntrack.Close()
		return
	}
	tracker.closeAllTracked()
}

func startMemoryMonitor(ctx context.Context, server *clashServer) io.Closer {
	if common.IsIOS() {
		return startIOSMemoryMonitor(ctx, server)
	}

	return startFixedMemoryMonitor(ctx)
}

func startFixedMemoryMonitor(ctx context.Context) io.Closer {
	limit := memmon.FixedLimit(defaultNonIOSMemLimitBytes)
	// Non-iOS uses the monitor for visibility only. Reclaim and admission
	// control remain disabled by passing a nil executor.
	monitor := memmon.New(
		memoryMonitorConfig(limit),
		memmon.NewSensor(limit),
		nil,
	)

	return runMonitor(ctx, monitor)
}

func startIOSMemoryMonitor(ctx context.Context, server *clashServer) io.Closer {
	limit := memmon.FixedLimit(monitorLimitBytes())

	// Use a dedicated sensor here. Sharing the monitor sensor would race its
	// reused runtime/metrics buffers.
	gate := memmon.NewAdmissionGate(
		memmon.AdmissionConfig{},
		memmon.NewSensor(limit),
		admissionRejectionHandler(server),
	)
	server.SetAdmissionGate(gate)

	reclaimer := &memoryReclaimer{tracker: server.connTracker}
	executor := memmon.NewExecutor(
		reclaimer,
		settings.GetString(settings.LogPathKey),
		common.Platform,
		common.Version,
		gate,
	)

	monitor := memmon.New(
		memoryMonitorConfig(limit),
		memmon.NewSensor(limit),
		executor,
	)

	return runMonitor(ctx, monitor)
}

func monitorLimitBytes() uint64 {
	mb := env.GetInt(env.MemoryLimitMB)
	switch {
	case mb <= 0:
		return defaultIOSMemLimitBytes
	case mb < minMemLimitMB:
		slog.Warn("Ignoring low memory monitor limit override", "mb", mb, "min_mb", minMemLimitMB)
	case mb > maxMemLimitMB:
		slog.Warn("Ignoring high memory monitor limit override", "mb", mb, "max_mb", maxMemLimitMB)
	default:
		return uint64(mb) * 1024 * 1024
	}
	return defaultIOSMemLimitBytes
}

func memoryMonitorConfig(limit memmon.LimitProvider) memmon.Config {
	cfg := memmon.Config{LimitProvider: limit}
	if !common.IsIOS() {
		// On Android/dev the governing signal includes the Go heap, so pin soft-enter to the
		// GOMEMLIMIT footprint and let GC act before eviction. iOS steers on dynamic headroom, which
		// already reflects whatever GC freed, so no clamp is wanted there.
		cfg.GoMemLimit = mobileMemoryLimit
	}
	return cfg
}

func admissionRejectionHandler(cs *clashServer) func(bool) {
	if !settings.GetBool(settings.AdmissionRejectionDisabledKey) {
		return cs.setRejectMode
	}

	slog.Warn("[DEV] Admission rejection disabled; connections will not be rejected under high memory pressure")
	return func(reject bool) {
		slog.Warn("[DEV] Rejection toggle fired (notice-only)", "reject", reject)
	}
}

func runMonitor(ctx context.Context, mon *memmon.Monitor) io.Closer {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)
		mon.Run(ctx)
	}()

	return closerFunc(func() error {
		cancel()
		<-done
		return nil
	})
}
