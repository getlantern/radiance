package scanner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCache_SaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scanner_cache.json")

	working := []Result{
		{Candidate: Candidate{Provider: "akamai", Domain: "a248.e.akamai.net", IPAddress: "23.47.48.230"}, Latency: 95 * time.Millisecond, Status: 200},
		{Candidate: Candidate{Provider: "cloudfront", Domain: "aa1.awsstatic.com", IPAddress: "99.84.2.4"}, Latency: 110 * time.Millisecond, Status: 200},
	}

	if err := SaveCache(path, working); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	got, err := LoadCache(path, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d; want 2", len(got))
	}
	if got[0].Candidate.Provider != "akamai" || got[1].Candidate.Provider != "cloudfront" {
		t.Errorf("order or content lost: %#v", got)
	}
}

func TestCache_MissingFileIsNotError(t *testing.T) {
	got, err := LoadCache("/nonexistent/path/that/cannot/exist", 24*time.Hour)
	if err != nil {
		t.Errorf("missing cache file should return (nil, nil); got err=%v", err)
	}
	if got != nil {
		t.Errorf("expected nil results, got %v", got)
	}
}

func TestCache_TTLFiltersStaleEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scanner_cache.json")

	if err := SaveCache(path, []Result{
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.2.3.4"}, Latency: 1 * time.Second, Status: 200},
	}); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}

	got, err := LoadCache(path, 1*time.Nanosecond)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("TTL didn't filter stale entries: %v", got)
	}
}

func TestCache_SaveSkipsNonOKResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scanner_cache.json")
	working := []Result{
		{Candidate: Candidate{Provider: "akamai", IPAddress: "1.2.3.4"}, Status: 200},
		{Candidate: Candidate{Provider: "akamai", IPAddress: "5.6.7.8"}, Status: 403},
	}
	if err := SaveCache(path, working); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) == "" || len(raw) == 0 {
		t.Fatalf("empty cache file")
	}
	got, _ := LoadCache(path, time.Hour)
	if len(got) != 1 {
		t.Errorf("expected 1 OK result saved, got %d", len(got))
	}
}

func TestCache_WrongVersionIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scanner_cache.json")
	os.WriteFile(path, []byte(`{"version":999,"working":[]}`), 0o644)
	_, err := LoadCache(path, time.Hour)
	if err == nil {
		t.Errorf("expected version mismatch error")
	}
}
