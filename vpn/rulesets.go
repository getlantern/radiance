package vpn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	C "github.com/sagernet/sing-box/constant"
	O "github.com/sagernet/sing-box/option"

	"github.com/getlantern/radiance/common/atomicfile"
)

const (
	ruleSetsCacheDir     = "rulesets"
	ruleSetDownloadLimit = 32 << 20 // 32 MiB — upper bound on a single rule-set file
	ruleSetDownloadTotal = 60 * time.Second
	defaultRuleSetTTL    = 24 * time.Hour
)

// preDownloadRuleSets rewrites every remote rule-set in opts to a local one,
// downloading content via Go's standard net/http (OS resolver, default dialer)
// into dataPath/rulesets. This sidesteps a bootstrap deadlock on Android where
// sing-box's DNS/dialers have no registered network interface at rule-set init
// time, so every hostname lookup fails with "no available network interface"
// — regardless of which DNS server the route chooses.
//
// Successful downloads are cached on disk and reused until UpdateInterval
// elapses. On download failure we fall back to any existing cached copy; if
// there is none we leave the entry as remote, preserving current behavior.
func preDownloadRuleSets(ctx context.Context, opts *O.Options, dataPath string) {
	if opts.Route == nil || len(opts.Route.RuleSet) == 0 {
		return
	}
	cacheDir := filepath.Join(dataPath, ruleSetsCacheDir)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		slog.Warn("Failed to create rule-set cache directory, skipping pre-download", "error", err)
		return
	}

	client := &http.Client{Timeout: ruleSetDownloadTotal}
	for i, rs := range opts.Route.RuleSet {
		if rs.Type != C.RuleSetTypeRemote {
			continue
		}
		url := rs.RemoteOptions.URL
		if url == "" {
			continue
		}

		path := ruleSetCachePath(cacheDir, rs.Tag, url)
		ttl := time.Duration(rs.RemoteOptions.UpdateInterval)
		if ttl <= 0 {
			ttl = defaultRuleSetTTL
		}

		if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) < ttl {
			slog.Debug("Using cached rule-set", "tag", rs.Tag, "path", path)
			opts.Route.RuleSet[i] = toLocalRuleSet(rs, path)
			continue
		}

		if err := downloadRuleSet(ctx, client, url, path); err != nil {
			if _, statErr := os.Stat(path); statErr == nil {
				slog.Warn("Rule-set download failed, using stale cache", "tag", rs.Tag, "error", err)
				opts.Route.RuleSet[i] = toLocalRuleSet(rs, path)
			} else {
				slog.Warn("Rule-set download failed, leaving as remote", "tag", rs.Tag, "error", err)
			}
			continue
		}
		slog.Info("Pre-downloaded rule-set", "tag", rs.Tag, "path", path)
		opts.Route.RuleSet[i] = toLocalRuleSet(rs, path)
	}
}

// ruleSetCachePath derives a stable per-URL path under cacheDir. We hash the
// URL so the filename is fixed-length and contains no untrusted path segments,
// and preserve .srs/.json as a hint to sing-box's format auto-detection.
func ruleSetCachePath(cacheDir, tag, url string) string {
	sum := sha256.Sum256([]byte(url))
	ext := filepath.Ext(url)
	switch ext {
	case ".srs", ".json":
	default:
		ext = ".srs"
	}
	name := fmt.Sprintf("%s-%s%s", tag, hex.EncodeToString(sum[:8]), ext)
	return filepath.Join(cacheDir, name)
}

func toLocalRuleSet(src O.RuleSet, path string) O.RuleSet {
	return O.RuleSet{
		Type:         C.RuleSetTypeLocal,
		Tag:          src.Tag,
		Format:       src.Format,
		LocalOptions: O.LocalRuleSet{Path: path},
	}
}

func downloadRuleSet(ctx context.Context, client *http.Client, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, ruleSetDownloadLimit+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) > ruleSetDownloadLimit {
		return fmt.Errorf("rule-set exceeds %d bytes", ruleSetDownloadLimit)
	}
	return atomicfile.WriteFile(path, body, 0644)
}
