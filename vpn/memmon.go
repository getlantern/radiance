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
	// defaultMemLimitBytes is the Android/dev byte budget, just under the ≈50 MB iOS extension cap.
	// iOS uses dynamic headroom when available and falls back to this budget otherwise.
	defaultMemLimitBytes = 48 * 1024 * 1024

	minMemLimitMB = 16
	maxMemLimitMB = 512
)

// memoryReclaimer implements memmon.Reclaimer. Soft eviction acts on the connection tracker
// (oldest-first); the hard path prefers conntrack.Close, which drains every dialed conn under one
// lock, and falls back to the tracker when conntrack is compiled out (without the with_conntrack
// build tag conntrack.Close is a no-op) so a hard reclaim is never silently lost.
type memoryReclaimer struct {
	ct *connTracker
}

func (r *memoryReclaimer) ConnectionsOldestFirst() []memmon.ConnectionRef {
	md := r.ct.Connections()
	refs := make([]memmon.ConnectionRef, len(md))
	for i, m := range md {
		refs[i] = memmon.ConnectionRef{ID: m.ID, CreatedAt: m.CreatedAt}
	}
	slices.SortFunc(refs, func(a, b memmon.ConnectionRef) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return refs
}

func (r *memoryReclaimer) CloseConn(id uuid.UUID) { r.ct.closeConn(id) }

func (r *memoryReclaimer) CloseAllConnections() { closeAllRouted(r.ct) }

// closeAllRouted force-closes every connection, preferring conntrack.Close (all dialed conns under
// one lock) and falling back to the tracker when conntrack is compiled out — so it is never a silent
// no-op regardless of the with_conntrack build tag. Shared by the hard reclaim and the mode-switch
// reset.
func closeAllRouted(ct *connTracker) {
	if conntrack.Enabled {
		conntrack.Close()
		return
	}
	ct.closeAllTracked()
}

func (r *memoryReclaimer) FreeOSMemory() { runtimeDebug.FreeOSMemory() }

func (r *memoryReclaimer) OpenConnectionCount() int { return int(r.ct.activeConnectionCount()) }

func (r *memoryReclaimer) TotalDialedConnections() int {
	if conntrack.Enabled {
		return conntrack.Count()
	}
	return int(r.ct.activeConnectionCount())
}

func startMemoryMonitor(ctx context.Context, cs *clashServer) io.Closer {
	limit := memmon.FixedLimit(monitorLimitBytes())
	cfg := memoryMonitorConfig(limit)

	// The gate samples a fresh footprint per admitted connection, so it needs its
	// own Sensor: sampling concurrently with the monitor's would race the reused
	// runtime/metrics buffers.
	gate := memmon.NewAdmissionGate(
		memmon.AdmissionConfig{},
		memmon.NewSensor(limit),
		admissionRejectionHandler(cs),
	)
	cs.SetAdmissionGate(gate)

	exec := memmon.NewExecutor(
		&memoryReclaimer{ct: cs.connTracker},
		settings.GetString(settings.LogPathKey),
		common.Platform,
		common.Version,
		gate,
	)

	mon := memmon.New(cfg, memmon.NewSensor(limit), exec)
	return runMonitor(ctx, mon)
}

func monitorLimitBytes() uint64 {
	mb := env.GetInt(env.MemoryLimitMB)
	switch {
	case mb <= 0:
		return defaultMemLimitBytes
	case mb < minMemLimitMB:
		slog.Warn("Ignoring low memory monitor limit override", "mb", mb, "min_mb", minMemLimitMB)
	case mb > maxMemLimitMB:
		slog.Warn("Ignoring high memory monitor limit override", "mb", mb, "max_mb", maxMemLimitMB)
	default:
		return uint64(mb) << 20
	}
	return defaultMemLimitBytes
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
