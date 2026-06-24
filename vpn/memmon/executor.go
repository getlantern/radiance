package memmon

import "time"

// executor is the reaction actuator. It is a pure actuator: it obeys the
// Decision's flags and never decides when to act — all timing (the hard edge,
// cooldown, dwell, and settle) lives in the DecisionEngine. All of its state is owned by
// the single monitor-loop goroutine, so it needs no locking; now is threaded in
// from the loop so the FreeOSMemory rate limit is deterministic.
type executor struct {
	reclaimer Reclaimer
	dump      *dumpWriter
	gate      *AdmissionGate
	cfg       reactionConfig

	lastLevel  PressureLevel
	dumped     bool
	lastFreeOS time.Time
}

// NewExecutor builds the reaction executor. An empty dumpDir disables crash
// dumps; a nil emit disables pressure events; a nil gate disables admission
// control. platform and version are stamped into the dump.
func NewExecutor(reclaimer Reclaimer, dumpDir, platform, version string, gate *AdmissionGate) Executor {
	return newExecutor(reclaimer, dumpDir, platform, version, gate, reactionConfig{})
}

func newExecutor(reclaimer Reclaimer, dumpDir, platform, version string, gate *AdmissionGate, config reactionConfig) Executor {
	var dump *dumpWriter
	if dumpDir != "" {
		dump = newDumpWriter(dumpDir, platform, version)
	}
	return &executor{reclaimer: reclaimer, dump: dump, gate: gate, cfg: config.applyDefaults(), lastLevel: LevelNormal}
}

func (e *executor) Apply(a Decision, now time.Time) {
	if a.WriteDump {
		// Written before reclaiming: the dump captures the live process at the
		// cliff, and a SIGKILL during reclaim must not lose the reason we were
		// about to act on.
		e.maybeDump(a, now)
	}
	switch {
	case a.CloseAllConnections:
		e.reclaimer.CloseAllConnections()
		e.freeOSMemoryRL(now)
	case a.Level == LevelSoft && a.EvictOldestBatch:
		e.softEvict()
		// The signal will not recede on its own after a soft eviction — freed
		// relay buffers sit in the bufpool and the scavenger releases lazily —
		// so a throttled FreeOSMemory is what lets the DecisionEngine observe a real drop
		// and exit Soft. Rate-limited, so not the per-tick STW cost.
		e.freeOSMemoryRL(now)
	}
	if a.Level != e.lastLevel {
		if a.Level <= LevelSoft {
			e.dumped = false // re-arm the per-episode dump once pressure recedes out of Hard
		}
		e.lastLevel = a.Level
	}
	if e.gate != nil {
		e.gate.Observe(a.Level, a.PressureRatio, now)
	}
}

func (e *executor) softEvict() {
	refs := e.reclaimer.ConnectionsOldestFirst()
	n := e.batchFor(len(refs))
	for _, c := range refs[:n] {
		e.reclaimer.CloseConn(c.ID)
	}
}

func (e *executor) batchFor(available int) int {
	if available == 0 {
		return 0
	}
	n := available / e.cfg.SoftDivisor
	n = max(n, 1)
	return min(n, available, e.cfg.SoftBatchMax)
}

// freeOSMemoryRL runs FreeOSMemory at most once per FreeOSMinInterval. The
// DecisionEngine's hard cooldown and settle window are the primary limiters; this
// single-flight gate is the backstop that guarantees no 4 Hz forced-GC spiral
// even if a reclaim flag were to re-fire.
func (e *executor) freeOSMemoryRL(now time.Time) {
	if now.Sub(e.lastFreeOS) < e.cfg.FreeOSMinInterval {
		return
	}
	e.lastFreeOS = now
	e.reclaimer.FreeOSMemory()
}

// maybeDump writes one crash dump per pressure episode (e.dumped is reset when
// the level recedes to at most Soft). The Go numbers come from the Decision's
// Snapshot (runtime/metrics, no ReadMemStats), so the whole path is
// stop-the-world-free.
func (e *executor) maybeDump(a Decision, now time.Time) {
	if e.dumped || e.dump == nil || a.Snapshot == nil {
		return
	}
	_ = e.dump.write(a, e.reclaimer.OpenConnectionCount(), e.reclaimer.TotalDialedConnections(), now)
	e.dumped = true
}
