package memmon

import (
	"context"
	"time"
)

// Config holds the sensing/decision tuning. Zero-valued fields take the
// defaults applied by applyDefaults.
type Config struct {
	// LimitProvider is the static byte ceiling for Android/dev (LimitProvider.Cap). On iOS
	// the Cap is dynamic and this is unused.
	LimitProvider LimitProvider
	// GoMemLimit pins the soft-enter threshold so it never maps below a footprint
	// of GOMEMLIMIT (GC acts before eviction). Leave 0 on iOS, where the dynamic
	// available-memory Cap is self-correcting and the clamp is not wanted.
	GoMemLimit uint64

	BaseInterval   time.Duration
	PredictHorizon time.Duration

	// SoftEnter, HardEnter, SoftExit, and HardExit are pressure thresholds in
	// [0,1]. The soft band is wide so eviction is gradual rather than a quick
	// step to Hard.
	SoftEnter, HardEnter, SoftExit, HardExit float64

	DwellSamples int
}

const (
	defaultSoftEnter      = 0.75
	defaultHardEnter      = 0.92
	defaultHardExit       = 0.85
	defaultSoftExit       = 0.65
	defaultBaseInterval   = 1 * time.Second
	defaultPredictHorizon = 5 * time.Second
	defaultDwellSamples   = 3
)

func (c Config) applyDefaults() Config {
	if c.SoftEnter == 0 {
		c.SoftEnter = defaultSoftEnter
	}
	if c.HardEnter == 0 {
		c.HardEnter = defaultHardEnter
	}
	if c.HardExit == 0 {
		c.HardExit = defaultHardExit
	}
	if c.SoftExit == 0 {
		c.SoftExit = defaultSoftExit
	}
	if c.BaseInterval <= 0 {
		c.BaseInterval = defaultBaseInterval
	}
	if c.PredictHorizon <= 0 {
		c.PredictHorizon = defaultPredictHorizon
	}
	if c.DwellSamples <= 0 {
		c.DwellSamples = defaultDwellSamples
	}
	return c
}

// Executor consumes each Decision and performs the reclamation. Implemented by
// the reaction side; now is passed so the executor's own rate-limiting is
// deterministic under test.
type Executor interface {
	Apply(a Decision, now time.Time)
}

// Monitor runs the single serial loop: each tick samples, decides, and applies
// inline on one goroutine. The only expensive reclaim operations are
// edge-triggered and rate-limited by the engine, so the rare inline pause during
// reclamation is acceptable and there is no separate sampler to starve.
type Monitor struct {
	engine       *DecisionEngine
	readSample   func(now time.Time) Sample
	executor     Executor
	baseInterval time.Duration
}

// New wires a Monitor from the decision config, the per-tick Sampler, and the
// executor.
func New(cfg Config, sampler Sampler, executor Executor) *Monitor {
	engine := NewDecisionEngine(cfg)
	return &Monitor{
		engine:       engine,
		readSample:   sampler.Sample,
		executor:     executor,
		baseInterval: engine.cfg.BaseInterval,
	}
}

// Run drives the loop until ctx is canceled. It blocks.
func (m *Monitor) Run(ctx context.Context) {
	interval := m.baseInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			decision := m.Step(now)
			interval = decision.NextInterval
			if interval <= 0 {
				interval = m.baseInterval
			}
			timer.Reset(interval)
		}
	}
}

// Step runs one sample→decide→apply cycle for the tick at now and returns the
// Decision. Run calls it on the timer; tests drive it directly with a synthetic
// clock and Sampler to exercise the loop without a real timer.
func (m *Monitor) Step(now time.Time) Decision {
	sample := m.readSample(now)
	decision := m.engine.Decide(sample)
	if m.executor != nil {
		m.executor.Apply(decision, now)
	}
	return decision
}
