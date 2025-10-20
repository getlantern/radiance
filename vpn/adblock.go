package vpn

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/radiance/common"
)

const (
	adBlockTag        = "ad-block"
	adBlockFile       = adBlockTag + ".json"
	adBlockMetaFile   = adBlockTag + ".meta.json"
	defaultFetchEvery = 24 * time.Hour
)

const emptyRuleSetJSON = `{"version":3,"rules":[]}`

type remoteMeta struct {
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	LastFetch    time.Time `json:"last_fetch,omitempty"`
}

type AdBlocker struct {
	ruleFile string
	metaFile string

	srcURL string
	every  time.Duration

	client  *http.Client
	enabled atomic.Bool

	mu   sync.Mutex
	stop chan struct{}
}

func NewAdBlockerHandler(client *http.Client, srcURL string, every time.Duration) (*AdBlocker, error) {
	path := filepath.Join(common.DataPath(), adBlockFile)
	meta := filepath.Join(common.DataPath(), adBlockMetaFile)

	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.WriteFile(path, []byte(emptyRuleSetJSON), 0644)
	}

	if client == nil {
		client = &http.Client{Timeout: common.DefaultHTTPTimeout}
	}
	if every <= 0 {
		every = defaultFetchEvery
	}

	a := &AdBlocker{
		ruleFile: path,
		metaFile: meta,
		srcURL:   srcURL,
		every:    every,
		client:   client,
		stop:     make(chan struct{}),
	}
	a.enabled.Store(a.fileHasRules())
	return a, nil
}

func (a *AdBlocker) Start(ctx context.Context) {
	t := time.NewTicker(a.every)
	go func() {
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-a.stop:
				return
			case <-t.C:
				_ = a.Refresh(ctx)
			}
		}
	}()
}

func (a *AdBlocker) Stop() { close(a.stop) }

func (a *AdBlocker) SetEnabled(enabled bool) error {
	if a.enabled.Swap(enabled) == enabled {
		return nil
	}
	if enabled {
		a.Refresh(context.Background())
		return nil
	}
	return os.WriteFile(a.ruleFile, []byte(emptyRuleSetJSON), 0644)
}

// Refresh pulls the latest ad blocking rules
func (a *AdBlocker) Refresh(ctx context.Context) error {
	if !a.enabled.Load() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.srcURL, nil)
	if err != nil {
		return err
	}
	meta := a.loadMeta()
	if meta.ETag != "" {
		req.Header.Set("If-None-Match", meta.ETag)
	}
	if meta.LastModified != "" {
		req.Header.Set("If-Modified-Since", meta.LastModified)
	}

	res, err := a.client.Do(req)
	if err != nil {
		slog.Warn("adBlock: fetch failed", "error", err)
		return nil
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotModified {
		return nil
	} else if res.StatusCode != http.StatusOK {
		slog.Warn("adBlock: unexpected status", "code", res.StatusCode)
		return nil
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if !isValidRuleSetJSON(body) {
		slog.Warn("adBlock: not a valid rule set, ignoring")
		return nil
	}
	if err := os.WriteFile(a.ruleFile, body, 0644); err != nil {
		return err
	}
	meta.ETag = res.Header.Get("ETag")
	meta.LastModified = res.Header.Get("Last-Modified")
	meta.LastFetch = time.Now()
	a.saveMeta(meta)

	return nil
}

func (a *AdBlocker) IsEnabled() bool {
	return a.enabled.Load() && a.fileHasRules()
}

// Helpers

func (a *AdBlocker) fileHasRules() bool {
	data, err := os.ReadFile(a.ruleFile)
	if err != nil {
		return false
	}
	return !bytes.Contains(data, []byte(`"rules":[]`))
}

func (a *AdBlocker) loadMeta() remoteMeta {
	var m remoteMeta
	if b, err := os.ReadFile(a.metaFile); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (a *AdBlocker) saveMeta(m remoteMeta) {
	b, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(a.metaFile, b, 0644)
}

type probe struct {
	Version int             `json:"version"`
	Rules   json.RawMessage `json:"rules"`
}

func isValidRuleSetJSON(b []byte) bool {
	var p probe
	if err := json.Unmarshal(b, &p); err != nil {
		return false
	}
	return p.Version == 3 && len(p.Rules) > 0 && !bytes.Equal(bytes.TrimSpace(p.Rules), []byte("[]"))
}
