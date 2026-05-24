package scanner

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/domainfront"
	tls "github.com/refraction-networking/utls"
)

// ServiceConfig configures a scanner Service.
type ServiceConfig struct {
	Config *domainfront.Config

	CacheFile string

	RefreshInterval  time.Duration // default 1h
	CacheTTL         time.Duration // default 6h, matches Samim's "time of day" observation
	MinWorkingFronts int           // re-scan when working count drops below this; default 3

	KnownSample      int
	CloudFrontSample int
	AkamaiSample     int

	Probe ProbeOptions

	Resolver Resolver
	Logger   *slog.Logger
}

// ProbeOptions is the Scanner's view of scanner.Options — every probe
// in the service uses the same dialer / TLS settings.
type ProbeOptions struct {
	Dialer        Dialer
	RootCAs       *x509.CertPool
	ClientHelloID tls.ClientHelloID
	DialTimeout   time.Duration
	Concurrency   int
}

func (c *ServiceConfig) defaults() {
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = 1 * time.Hour
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = 6 * time.Hour
	}
	if c.MinWorkingFronts <= 0 {
		c.MinWorkingFronts = 3
	}
	if c.Probe.DialTimeout <= 0 {
		c.Probe.DialTimeout = 5 * time.Second
	}
	if c.Probe.Concurrency <= 0 {
		c.Probe.Concurrency = 8
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Service maintains a per-client working-front list, refreshing it on a
// schedule and on demand. Pick returns the next-best working front;
// ReportFailure demotes a front so subsequent Picks skip it and the
// next refresh runs sooner.
type Service struct {
	cfg ServiceConfig

	mu       sync.Mutex
	working  []Result
	pickIdx  int
	failures map[string]int

	refreshSignal chan struct{}
	refreshing    atomic.Bool

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	started  atomic.Bool
}

// NewService loads the on-disk cache if present and returns a Service
// ready to start. Start kicks off the background refresh loop.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Config == nil {
		return nil, errors.New("scanner: ServiceConfig.Config required")
	}
	cfg.defaults()

	s := &Service{
		cfg:           cfg,
		failures:      make(map[string]int),
		refreshSignal: make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}

	if cfg.CacheFile != "" {
		cached, err := LoadCache(cfg.CacheFile, cfg.CacheTTL)
		if err != nil {
			cfg.Logger.Warn("scanner: cache load failed", slog.Any("error", err))
		} else if len(cached) > 0 {
			s.working = cached
			cfg.Logger.Info("scanner: cache loaded", slog.Int("count", len(cached)))
		}
	}
	return s, nil
}

// Start runs an initial refresh and the periodic loop. Returns when ctx
// is canceled or Close is called. Safe to call once.
func (s *Service) Start(ctx context.Context) {
	s.started.Store(true)
	defer close(s.done)
	go s.refresh(ctx)

	t := time.NewTicker(s.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			go s.refresh(ctx)
		case <-s.refreshSignal:
			go s.refresh(ctx)
		}
	}
}

// Close stops the background loop. Idempotent. Safe to call before
// Start — in that case it just marks the Service stopped without
// waiting on a loop that was never running.
func (s *Service) Close() error {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.started.Load() {
		<-s.done
	}
	return nil
}

// Working returns a snapshot of the current working front list ordered
// by latency.
func (s *Service) Working() []Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Result, len(s.working))
	copy(out, s.working)
	return out
}

// Pick returns the next working front in round-robin order so all
// fronts get traffic instead of every dial pinning to the lowest-
// latency one (which is what would happen with naive head-of-list).
// Returns false when the working list is empty; callers should then
// either wait for a refresh or trigger one via Refresh.
func (s *Service) Pick() (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.working) == 0 {
		return Result{}, false
	}
	r := s.working[s.pickIdx%len(s.working)]
	s.pickIdx++
	return r, true
}

// ReportFailure tells the Service a front returned by Pick subsequently
// stopped working. Tracking is per-(provider, IP, SNI); after two
// failures within a refresh cycle the front is removed from the
// working list. A scheduled refresh is signaled when the working list
// drops below MinWorkingFronts.
func (s *Service) ReportFailure(c Candidate) {
	key := failureKey(c)
	s.mu.Lock()
	s.failures[key]++
	count := s.failures[key]
	if count >= 2 {
		s.removeLocked(c)
	}
	lowWater := len(s.working) < s.cfg.MinWorkingFronts
	s.mu.Unlock()

	if lowWater {
		s.signalRefresh()
	}
}

// Refresh triggers an out-of-band scan, returning immediately. The
// refresh runs on the Service's goroutine; the resulting working list
// is observable via Working / Pick after it completes. Multiple calls
// while a refresh is already in flight are coalesced.
func (s *Service) Refresh() { s.signalRefresh() }

func (s *Service) signalRefresh() {
	select {
	case s.refreshSignal <- struct{}{}:
	default:
	}
}

func (s *Service) refresh(ctx context.Context) {
	if !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	defer s.refreshing.Store(false)

	cands, err := BuildPool(ctx, PoolOptions{
		Config:           s.cfg.Config,
		KnownSample:      s.cfg.KnownSample,
		CloudFrontSample: s.cfg.CloudFrontSample,
		AkamaiSample:     s.cfg.AkamaiSample,
		Resolver:         s.cfg.Resolver,
	})
	if err != nil {
		s.cfg.Logger.Warn("scanner: build pool failed", slog.Any("error", err))
		return
	}
	if len(cands) == 0 {
		s.cfg.Logger.Warn("scanner: empty pool, skipping scan")
		return
	}

	s.cfg.Logger.Info("scanner: scanning", slog.Int("candidates", len(cands)))
	start := time.Now()
	results := Scan(ctx, cands, Options{
		Dialer:        s.cfg.Probe.Dialer,
		RootCAs:       s.cfg.Probe.RootCAs,
		ClientHelloID: s.cfg.Probe.ClientHelloID,
		DialTimeout:   s.cfg.Probe.DialTimeout,
		Concurrency:   s.cfg.Probe.Concurrency,
	})
	working := RankWorking(results)
	elapsed := time.Since(start)
	s.cfg.Logger.Info("scanner: scan complete",
		slog.Int("working", len(working)),
		slog.Int("total", len(results)),
		slog.Duration("elapsed", elapsed),
	)

	s.mu.Lock()
	s.working = working
	s.pickIdx = 0
	s.failures = make(map[string]int)
	s.mu.Unlock()

	if s.cfg.CacheFile != "" {
		if err := SaveCache(s.cfg.CacheFile, working); err != nil {
			s.cfg.Logger.Warn("scanner: cache save failed", slog.Any("error", err))
		}
	}
}

func (s *Service) removeLocked(c Candidate) {
	key := failureKey(c)
	filtered := s.working[:0]
	for _, r := range s.working {
		if failureKey(r.Candidate) == key {
			continue
		}
		filtered = append(filtered, r)
	}
	s.working = filtered
}

func failureKey(c Candidate) string {
	return fmt.Sprintf("%s|%s|%s", c.Provider, c.IPAddress, c.SNI)
}
