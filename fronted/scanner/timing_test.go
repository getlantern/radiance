package scanner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/domainfront"
)

// TestLive_TimeToFirstWorking measures real-world scan latency and
// time-to-first-working-front against live CDN infrastructure. Opt-in
// (SCANNER_INTEGRATION=1) — exercises the production probe path
// end-to-end.
//
// Reported metrics:
//   - time-to-first-working: how soon after the scan starts does any
//     candidate complete OK (the most operationally relevant number —
//     this is how long the user waits before the first front is usable)
//   - p50/p90 working-result latency: the per-probe RTT distribution
//   - total scan wall time: when does the last probe finish
//   - hit rate per feeder
func TestLive_TimeToFirstWorking(t *testing.T) {
	integrationGate(t)

	cfg, err := loadProductionConfig(t)
	if err != nil {
		t.Fatalf("loadProductionConfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool, err := BuildPool(ctx, PoolOptions{
		Config:           cfg,
		KnownSample:      50,
		CloudFrontSample: 10,
		AkamaiSample:     5,
	})
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	t.Logf("pool: %d candidates (50 known + Akamai-DNS-resolved + 10 CloudFront-random)", len(pool))

	rootCAs, err := TrustedCAsPool(cfg)
	if err != nil {
		t.Fatalf("TrustedCAsPool: %v", err)
	}

	start := time.Now()
	var firstWorking int64
	results := scanWithFirstHookCB(ctx, pool, Options{
		RootCAs:     rootCAs,
		Concurrency: 8,
		DialTimeout: 5 * time.Second,
	}, func() {
		atomic.CompareAndSwapInt64(&firstWorking, 0, int64(time.Since(start)))
	})
	elapsed := time.Since(start)
	working := RankWorking(results)

	t.Logf("scan complete: %d/%d working in %s", len(working), len(results), elapsed.Round(time.Millisecond))
	if firstWorking > 0 {
		t.Logf("time to first working front: %s", time.Duration(firstWorking).Round(time.Millisecond))
	}

	byProvider := map[string]struct{ ok, total int }{}
	for _, r := range results {
		stats := byProvider[r.Candidate.Provider]
		stats.total++
		if r.OK() {
			stats.ok++
		}
		byProvider[r.Candidate.Provider] = stats
	}
	for prov, s := range byProvider {
		t.Logf("  %s: %d/%d working (%.0f%%)", prov, s.ok, s.total, 100*float64(s.ok)/float64(s.total))
	}

	if len(working) > 0 {
		latencies := make([]time.Duration, len(working))
		for i, r := range working {
			latencies[i] = r.Latency
		}
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		p50 := latencies[len(latencies)/2]
		p90 := latencies[(len(latencies)*9)/10]
		t.Logf("working-result latency: p50=%s p90=%s min=%s", p50.Round(time.Millisecond), p90.Round(time.Millisecond), latencies[0].Round(time.Millisecond))
	}

	if len(working) == 0 {
		t.Errorf("0 working fronts after full scan; expected at least 1 against live CDN")
	}
}

// scanWithFirstHookCB is Scan with a callback fired exactly once on the
// first OK result. Used to time how soon the user could start using
// the scanner output rather than waiting for the full scan to finish.
func scanWithFirstHookCB(ctx context.Context, candidates []Candidate, opts Options, onFirst func()) []Result {
	opts.defaults()
	results := make([]Result, len(candidates))
	if len(candidates) == 0 {
		return results
	}
	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	var once sync.Once
	for i, c := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c Candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				results[i] = Result{Candidate: c, Err: err}
				return
			}
			r := Probe(ctx, c, opts)
			if r.OK() && onFirst != nil {
				once.Do(onFirst)
			}
			results[i] = r
		}(i, c)
	}
	wg.Wait()
	return results
}

// loadProductionConfig reads the embedded radiance fronted.yaml.gz
// (the same config the radiance client uses in production) so the
// timing test exercises a realistic pool.
func loadProductionConfig(t *testing.T) (*domainfront.Config, error) {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "..", "..", "kindling", "fronted", "fronted.yaml.gz")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return domainfront.ParseConfig(raw)
}
