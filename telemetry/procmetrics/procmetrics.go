// Package procmetrics emits process resident-memory, CPU, and Go runtime
// metrics for the process it runs in.
//
// It must be started from the sing-box process. On macOS/iOS the tunnel runs
// in the network-extension process, distinct from the control process, and
// the proxy's real cost lives there. RSS in particular is the only signal
// that captures the WATER WASM outbound's linear memory, which wazero maps
// outside the Go heap and so never appears in runtime.MemStats.
package procmetrics

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"

	"github.com/shirou/gopsutil/v4/process"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const meterName = "github.com/getlantern/radiance/metrics"

var startOnce sync.Once

// Start registers the metric instruments once per process; later calls are
// no-ops, so it is safe to call on every tunnel (re)connect. Values flow
// through the global OpenTelemetry meter provider, so the instruments stay
// inert until telemetry is configured.
func Start() {
	startOnce.Do(func() {
		if err := register(); err != nil {
			slog.Warn("failed to register process metrics", "error", err)
		}
	})
}

func register() error {
	meter := otel.Meter(meterName)

	rss, err := meter.Int64ObservableGauge("process.memory.usage",
		metric.WithUnit("By"),
		metric.WithDescription("Resident set size; includes off-heap memory such as the WATER WASM runtime."))
	if err != nil {
		return err
	}
	cpuTime, err := meter.Float64ObservableCounter("process.cpu.time",
		metric.WithUnit("s"),
		metric.WithDescription("CPU time consumed by the process, split by user and system state."))
	if err != nil {
		return err
	}
	heapInuse, err := meter.Int64ObservableGauge("process.runtime.go.mem.heap_inuse", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	heapAlloc, err := meter.Int64ObservableGauge("process.runtime.go.mem.heap_alloc", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	sys, err := meter.Int64ObservableGauge("process.runtime.go.mem.sys", metric.WithUnit("By"))
	if err != nil {
		return err
	}
	goroutines, err := meter.Int64ObservableGauge("process.runtime.go.goroutines")
	if err != nil {
		return err
	}
	gcCount, err := meter.Int64ObservableCounter("process.runtime.go.gc.count")
	if err != nil {
		return err
	}

	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return err
	}
	userState := metric.WithAttributes(attribute.String("process.cpu.state", "user"))
	systemState := metric.WithAttributes(attribute.String("process.cpu.state", "system"))

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		if mi, err := proc.MemoryInfoWithContext(ctx); err == nil {
			o.ObserveInt64(rss, int64(mi.RSS))
		}
		if t, err := proc.TimesWithContext(ctx); err == nil {
			o.ObserveFloat64(cpuTime, t.User, userState)
			o.ObserveFloat64(cpuTime, t.System, systemState)
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		o.ObserveInt64(heapInuse, int64(m.HeapInuse))
		o.ObserveInt64(heapAlloc, int64(m.HeapAlloc))
		o.ObserveInt64(sys, int64(m.Sys))
		o.ObserveInt64(goroutines, int64(runtime.NumGoroutine()))
		o.ObserveInt64(gcCount, int64(m.NumGC))
		return nil
	}, rss, cpuTime, heapInuse, heapAlloc, sys, goroutines, gcCount)
	return err
}
