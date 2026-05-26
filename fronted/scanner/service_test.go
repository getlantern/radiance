package scanner

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/domainfront"
)

func TestService_PickRoundRobin(t *testing.T) {
	s := newServiceWithWorking(t, []Result{
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.1.1.1"}, Status: 200},
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.1.1.2"}, Status: 200},
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.1.1.3"}, Status: 200},
	})

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		r, ok := s.Pick()
		if !ok {
			t.Fatalf("Pick #%d returned !ok", i)
		}
		seen[r.Candidate.IPAddress]++
	}
	for ip, n := range seen {
		if n != 2 {
			t.Errorf("expected each IP picked twice, %s got %d", ip, n)
		}
	}
}

func TestService_PickEmptyReturnsFalse(t *testing.T) {
	s := newServiceWithWorking(t, nil)
	_, ok := s.Pick()
	if ok {
		t.Errorf("Pick on empty list should return false")
	}
}

func TestService_InsertSortedLockedMaintainsLatencyOrder(t *testing.T) {
	s := newServiceWithWorking(t, nil)
	for _, d := range []time.Duration{50, 10, 30, 20, 40} {
		s.insertSortedLocked(Result{
			Candidate: Candidate{IPAddress: d.String()},
			Latency:   d * time.Millisecond,
			Status:    200,
		})
	}
	got := s.Working()
	for i := 1; i < len(got); i++ {
		if got[i-1].Latency > got[i].Latency {
			t.Errorf("not sorted at %d: %v > %v", i, got[i-1].Latency, got[i].Latency)
		}
	}
	if len(got) != 5 {
		t.Errorf("len = %d; want 5", len(got))
	}
}

func TestScan_OnResultCalledPerCandidate(t *testing.T) {
	// Unroutable TEST-NET-1 addresses fail the TCP dial fast, so every
	// probe completes quickly; we only assert OnResult fires once each.
	cands := []Candidate{
		{Provider: "akamai", IPAddress: "192.0.2.1", TestURL: "https://x/ping", InnerHost: "x"},
		{Provider: "akamai", IPAddress: "192.0.2.2", TestURL: "https://x/ping", InnerHost: "x"},
		{Provider: "akamai", IPAddress: "192.0.2.3", TestURL: "https://x/ping", InnerHost: "x"},
	}
	var mu sync.Mutex
	count := 0
	results := Scan(context.Background(), cands, Options{
		DialTimeout: 500 * time.Millisecond,
		Concurrency: 3,
		OnResult: func(Result) {
			mu.Lock()
			count++
			mu.Unlock()
		},
	})
	if len(results) != 3 {
		t.Fatalf("results len = %d; want 3", len(results))
	}
	if count != 3 {
		t.Errorf("OnResult called %d times; want 3 (once per candidate)", count)
	}
}

func TestService_ReportFailureRemovesAfterTwo(t *testing.T) {
	bad := Candidate{Provider: "akamai", IPAddress: "1.1.1.1"}
	good := Candidate{Provider: "akamai", IPAddress: "2.2.2.2"}
	s := newServiceWithWorking(t, []Result{
		{Candidate: bad, Status: 200},
		{Candidate: good, Status: 200},
	})

	s.ReportFailure(bad)
	if len(s.Working()) != 2 {
		t.Errorf("first failure should not remove; got working=%d", len(s.Working()))
	}
	s.ReportFailure(bad)
	if len(s.Working()) != 1 {
		t.Errorf("second failure should remove; got working=%d", len(s.Working()))
	}
	if s.Working()[0].Candidate.IPAddress != "2.2.2.2" {
		t.Errorf("wrong candidate remained: %v", s.Working()[0].Candidate)
	}
}

func TestService_ReportFailureSignalsRefreshAtLowWater(t *testing.T) {
	s := newServiceWithWorking(t, []Result{
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.1.1.1"}, Status: 200},
	})
	s.cfg.MinWorkingFronts = 2

	s.ReportFailure(Candidate{Provider: "akamai", IPAddress: "1.1.1.1"})
	s.ReportFailure(Candidate{Provider: "akamai", IPAddress: "1.1.1.1"})

	select {
	case <-s.refreshSignal:
	default:
		t.Errorf("expected refresh signal after working dropped below MinWorkingFronts")
	}
}

func TestService_LoadsFromCacheOnConstruct(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "scanner_cache.json")
	SaveCache(cachePath, []Result{
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.2.3.4"}, Latency: 50 * time.Millisecond, Status: 200},
	})

	s, err := NewService(ServiceConfig{
		Config:    &domainfront.Config{},
		CacheFile: cachePath,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	w := s.Working()
	if len(w) != 1 {
		t.Errorf("expected 1 loaded from cache, got %d", len(w))
	}
}

func TestService_NoConfigIsError(t *testing.T) {
	_, err := NewService(ServiceConfig{})
	if err == nil {
		t.Errorf("expected error when Config is nil")
	}
}

func TestBuildPool_KnownOnly(t *testing.T) {
	cfg := &domainfront.Config{
		Providers: map[string]*domainfront.Provider{
			"akamai": {
				TestURL: "https://akamai.test/ping",
				Masquerades: []*domainfront.Masquerade{
					{Domain: "a248.e.akamai.net", IpAddress: "1.1.1.1"},
					{Domain: "a248.e.akamai.net", IpAddress: "1.1.1.2"},
					{Domain: "a248.e.akamai.net", IpAddress: "1.1.1.3"},
				},
			},
		},
	}
	got, err := BuildPool(context.Background(), PoolOptions{
		Config:      cfg,
		KnownSample: 2,
	})
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 sampled, got %d", len(got))
	}
}

func TestBuildPool_CloudFrontRawRange(t *testing.T) {
	cfg := &domainfront.Config{
		Providers: map[string]*domainfront.Provider{
			"cloudfront": {
				TestURL: "https://cf.test/ping",
				Masquerades: []*domainfront.Masquerade{
					{Domain: "aa1.awsstatic.com", IpAddress: "99.84.2.4"},
				},
			},
		},
	}
	got, err := BuildPool(context.Background(), PoolOptions{
		Config:           cfg,
		CloudFrontSample: 5,
	})
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("expected 5 raw-range samples (KnownSample=0 skips known), got %d", len(got))
	}
}

func TestBuildPool_KnownOptedIn(t *testing.T) {
	cfg := &domainfront.Config{
		Providers: map[string]*domainfront.Provider{
			"cloudfront": {
				TestURL: "https://cf.test/ping",
				Masquerades: []*domainfront.Masquerade{
					{Domain: "aa1.awsstatic.com", IpAddress: "99.84.2.4"},
					{Domain: "advertising.amazon.com", IpAddress: "3.164.130.9"},
				},
			},
		},
	}
	got, err := BuildPool(context.Background(), PoolOptions{
		Config:           cfg,
		KnownSample:      10,
		CloudFrontSample: 3,
	})
	if err != nil {
		t.Fatalf("BuildPool: %v", err)
	}
	if len(got) != 5 {
		t.Errorf("expected 2 known + 3 raw = 5, got %d", len(got))
	}
}

// --- helpers ---

func newServiceWithWorking(t *testing.T, working []Result) *Service {
	t.Helper()
	s, err := NewService(ServiceConfig{Config: &domainfront.Config{}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	s.working = working
	return s
}
