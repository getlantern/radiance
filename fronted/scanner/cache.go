package scanner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

const cacheSchemaVersion = 1

type cacheFile struct {
	Version   int           `json:"version"`
	UpdatedAt time.Time     `json:"updated_at"`
	Working   []cacheEntry  `json:"working"`
}

type cacheEntry struct {
	Candidate  Candidate     `json:"candidate"`
	Latency    time.Duration `json:"latency"`
	Status     int           `json:"status"`
	VerifiedAt time.Time     `json:"verified_at"`
}

// LoadCache reads a cache file and returns the working results. Returns
// (nil, nil) when the file doesn't exist — first-boot is not an error.
// Returns an error only on malformed contents.
//
// Entries older than ttl are filtered out so a stale cache from days
// ago doesn't seed the live pool with already-blocked IPs. ttl <= 0
// disables the filter (load everything).
func LoadCache(path string, ttl time.Duration) ([]Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cache: %w", err)
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode cache: %w", err)
	}
	if f.Version != cacheSchemaVersion {
		return nil, fmt.Errorf("cache schema %d unsupported (want %d)", f.Version, cacheSchemaVersion)
	}

	now := time.Now()
	out := make([]Result, 0, len(f.Working))
	for _, e := range f.Working {
		if ttl > 0 && now.Sub(e.VerifiedAt) > ttl {
			continue
		}
		out = append(out, Result{
			Candidate: e.Candidate,
			Latency:   e.Latency,
			Status:    e.Status,
		})
	}
	return out, nil
}

// SaveCache atomically writes the working results to path. The write is
// best-effort — a failed save is logged by the caller but doesn't
// affect runtime correctness.
func SaveCache(path string, working []Result) error {
	now := time.Now()
	f := cacheFile{
		Version:   cacheSchemaVersion,
		UpdatedAt: now,
		Working:   make([]cacheEntry, 0, len(working)),
	}
	for _, r := range working {
		if !r.OK() {
			continue
		}
		f.Working = append(f.Working, cacheEntry{
			Candidate:  r.Candidate,
			Latency:    r.Latency,
			Status:     r.Status,
			VerifiedAt: now,
		})
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename cache: %w", err)
	}
	return nil
}
