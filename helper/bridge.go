package helper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/helper/reverse"
)

const (
	defaultBridgePollInterval = 150 * time.Second
	bridgeDisabledWait        = time.Hour
	maxBridgeBackoff          = 10 * time.Minute
	bridgeCloseTimeout        = 10 * time.Second
	bridgeTagPrefix           = "bridge-"
)

// BridgeBox is the sing-box instance, outside the VPN tunnel, that runs a
// helper's bridges.
type BridgeBox interface {
	Start() error
	Close() error
	// SetBridges makes the box run exactly bridges, keyed by tag, replacing a
	// bridge whose options changed.
	SetBridges(bridges map[string]*lbO.SamizdatBridgeInboundOptions) error
}

// BridgeConfig plumbs in a BridgeClient's dependencies. A zero PollInterval
// and a nil Now fall back to production defaults.
type BridgeConfig struct {
	API  *ReverseAPI
	Keys interface {
		Get(pairingID string) ([]byte, bool)
		IDs(role string) []string
	}
	// NewBox builds a stopped box from lantern-cloud's bridge config. The box
	// calls count with a bridge's tag and the bytes it relays.
	NewBox       func(ctx context.Context, bridgeConfigJSON string, count func(tag string, n int64)) (BridgeBox, error)
	Usage        *Usage
	Now          func() time.Time
	PollInterval time.Duration
}

// BridgeClient serves this device's inviters while it volunteers: it polls
// lantern-cloud for their sealed reverse routes and runs one samizdat-bridge
// to each, counting the bytes relayed into Usage. It is safe for concurrent
// use.
type BridgeClient struct {
	cfg BridgeConfig

	mu     sync.Mutex
	cancel context.CancelFunc
	// done closes when the last started run has closed its box.
	done chan struct{}

	tagsMu sync.RWMutex
	// pairingByTag is never pruned, so bytes a removed bridge relays late are
	// still credited. It holds one entry per pairing seen.
	pairingByTag map[string]string

	// st is owned by the run goroutine; a run starts only after the previous
	// one's done closes.
	st bridgeState
}

type bridgeState struct {
	box       BridgeBox
	boxConfig string
	backoff   time.Duration
	lastSkips routeSkips
}

// routeSkips counts the routes a poll could not use, by reason.
type routeSkips struct {
	noKey, bad, stale, future, other int
}

// NewBridgeClient returns a stopped BridgeClient.
func NewBridgeClient(cfg BridgeConfig) *BridgeClient {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = defaultBridgePollInterval
	}
	return &BridgeClient{cfg: cfg, pairingByTag: make(map[string]string)}
}

// BridgeTag returns the tag of the bridge serving pairingID. It is a pseudonym
// of the pairing and must never be logged.
func BridgeTag(pairingID string) string {
	sum := sha256.Sum256([]byte(pairingID))
	return bridgeTagPrefix + hex.EncodeToString(sum[:])[:12]
}

// SetRunning starts or stops polling and the bridges, returning at once. A run
// ends when ctx does. Stopping closes the box in the background; a later start
// waits for that.
func (b *BridgeClient) SetRunning(ctx context.Context, running bool) {
	if b.cfg.API == nil || b.cfg.Keys == nil || b.cfg.NewBox == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !running {
		if b.cancel != nil {
			b.cancel()
			b.cancel = nil
		}
		return
	}
	if b.cancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	prev, done := b.done, make(chan struct{})
	b.cancel, b.done = cancel, done
	go b.run(runCtx, prev, done)
}

// Stop stops the client and waits for its box to close or for ctx, whichever
// comes first.
func (b *BridgeClient) Stop(ctx context.Context) {
	b.SetRunning(ctx, false)
	b.mu.Lock()
	done := b.done
	b.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (b *BridgeClient) run(ctx context.Context, prev <-chan struct{}, done chan struct{}) {
	defer close(done)
	if prev != nil {
		<-prev
	}
	defer b.closeBox()
	for {
		wait := b.cycle(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// cycle does one poll and returns how long to wait before the next.
func (b *BridgeClient) cycle(ctx context.Context) time.Duration {
	s := &b.st
	// Only a device with volunteer pairings has routes to fetch.
	ids := b.cfg.Keys.IDs(string(helperticket.RoleVolunteer))
	if len(ids) == 0 {
		b.closeBox()
		return b.cfg.PollInterval
	}
	resp, err := b.cfg.API.Routes(ctx)
	if err != nil {
		return b.routesFailed(ctx, err)
	}
	s.backoff = 0
	bridges := b.bridgesFor(resp.Routes, ids, b.cfg.Now())
	// Without the config's egress rules a bridge would reach anything the
	// helper's network can, so none runs.
	if len(bridges) == 0 || resp.BridgeConfig == "" {
		b.closeBox()
		return b.cfg.PollInterval
	}
	if s.box != nil && s.boxConfig != resp.BridgeConfig {
		b.closeBox()
	}
	if s.box == nil {
		box, err := b.cfg.NewBox(ctx, resp.BridgeConfig, b.count)
		if err != nil {
			slog.Warn("bridge: could not build the box", "err", err)
			return b.cfg.PollInterval
		}
		if err := box.Start(); err != nil {
			slog.Warn("bridge: could not start the box", "err", err)
			closeBridgeBox(box)
			return b.cfg.PollInterval
		}
		s.box, s.boxConfig = box, resp.BridgeConfig
	}
	// The errors name tags.
	if err := s.box.SetBridges(bridges); err != nil {
		slog.Warn("bridge: could not apply every bridge")
	}
	return b.cfg.PollInterval
}

// bridgesFor opens each route of a pairing in volunteerIDs and returns its
// bridge options by tag.
func (b *BridgeClient) bridgesFor(routes []ReverseBlob, volunteerIDs []string, now time.Time) map[string]*lbO.SamizdatBridgeInboundOptions {
	volunteer := make(map[string]bool, len(volunteerIDs))
	for _, id := range volunteerIDs {
		volunteer[id] = true
	}
	epoch := reverse.Epoch(now)
	bridges := make(map[string]*lbO.SamizdatBridgeInboundOptions)
	var skips routeSkips
	for _, rb := range routes {
		key, ok := b.cfg.Keys.Get(rb.PairingID)
		if !volunteer[rb.PairingID] || !ok {
			skips.noKey++
			continue
		}
		route, err := reverse.Open(key, rb.PairingID, rb.Blob, now)
		switch {
		case errors.Is(err, reverse.ErrBlob):
			skips.bad++
			continue
		case errors.Is(err, reverse.ErrStale):
			skips.stale++
			continue
		case errors.Is(err, reverse.ErrFuture):
			skips.future++
			continue
		case err != nil:
			skips.other++
			continue
		}
		sid := reverse.ShortID(key, epoch)
		addrs := make([]string, 0, len(route.Addrs))
		for _, ap := range route.Addrs {
			addrs = append(addrs, ap.String())
		}
		tag := BridgeTag(rb.PairingID)
		bridges[tag] = &lbO.SamizdatBridgeInboundOptions{
			Addrs:          addrs,
			PublicKey:      hex.EncodeToString(route.PublicKey[:]),
			ShortID:        hex.EncodeToString(sid[:]),
			ServerName:     route.CoverDomain,
			CertSPKISHA256: hex.EncodeToString(route.CertSPKISHA256[:]),
		}
		b.tagsMu.Lock()
		b.pairingByTag[tag] = rb.PairingID
		b.tagsMu.Unlock()
	}
	if skips != b.st.lastSkips && skips != (routeSkips{}) {
		slog.Info("bridge: skipped reverse routes", "no_key", skips.noKey, "bad", skips.bad,
			"stale", skips.stale, "future", skips.future, "other", skips.other)
	}
	b.st.lastSkips = skips
	return bridges
}

func (b *BridgeClient) routesFailed(ctx context.Context, err error) time.Duration {
	s := &b.st
	if ctx.Err() != nil {
		return b.cfg.PollInterval
	}
	var apiErr *ReverseAPIError
	switch {
	case errors.Is(err, ErrReverseDisabled):
		slog.Info("bridge: reverse routes disabled by lantern-cloud")
		b.closeBox()
		return bridgeDisabledWait
	case errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable):
		s.backoff = min(2*max(s.backoff, b.cfg.PollInterval), maxBridgeBackoff)
		slog.Info("bridge: lantern-cloud is busy, backing off", "wait", s.backoff)
		return s.backoff
	}
	slog.Warn("bridge: could not fetch reverse routes", "err", err)
	return b.cfg.PollInterval
}

// count credits n bytes relayed by the bridge tagged tag to its pairing.
func (b *BridgeClient) count(tag string, n int64) {
	if n <= 0 {
		return
	}
	b.tagsMu.RLock()
	pairing := b.pairingByTag[tag]
	b.tagsMu.RUnlock()
	b.cfg.Usage.Add(pairing, uint64(n))
}

func (b *BridgeClient) closeBox() {
	s := &b.st
	if s.box == nil {
		return
	}
	closeBridgeBox(s.box)
	s.box, s.boxConfig = nil, ""
}

// closeBridgeBox closes box, waiting at most bridgeCloseTimeout for it to
// drain its connections.
func closeBridgeBox(box BridgeBox) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := box.Close(); err != nil {
			slog.Warn("bridge: could not close the box")
		}
	}()
	select {
	case <-done:
	case <-time.After(bridgeCloseTimeout):
		slog.Warn("bridge: box close still draining connections; continuing")
	}
}
