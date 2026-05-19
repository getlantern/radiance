package scanner

import (
	"context"
	"path/filepath"
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
