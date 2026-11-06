package peer

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/portforward"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"

	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/helper/reverse"
)

const (
	defaultReverseRegisterInterval = 150 * time.Second
	defaultReverseRotateInterval   = time.Hour
	defaultReverseAddrMemory       = time.Hour
	reverseDisabledWait            = time.Hour
	maxReverseBackoff              = 10 * time.Minute
	// maxReverseRoutes is the most routes lantern-cloud accepts in one
	// register call.
	maxReverseRoutes = 20
	// maxReverseReregisters bounds the registers a cycle repeats because the
	// observed address changed, so two addresses alternating between calls
	// cannot loop.
	maxReverseReregisters  = 2
	reversePortDescription = "Lantern personal proxy"
)

// ReverseBox is the VPN box the reverse client serves through; vpn.VPNClient
// implements it.
type ReverseBox interface {
	SetReversePortal(cfg *samizdatreverse.PortalConfig, masqueradeDomain string) error
	SetReverseOutbounds(tags []string) error
	RebindReversePortal() error
}

// PairingKeySource returns this device's helper pairing keys; helper.PairingKeys
// implements it.
type PairingKeySource interface {
	Get(pairingID string) ([]byte, bool)
	IDs(role string) []string
}

// ReverseConfig plumbs in a ReverseClient's dependencies. Zero durations and a
// nil Now or NewForwarder fall back to production defaults.
type ReverseConfig struct {
	API          *helper.ReverseAPI
	Keys         PairingKeySource
	Box          ReverseBox
	NewForwarder func(ctx context.Context) (portForwarder, error)
	// OnTags, if set, is given a copy of the pairing ID to helper tag map each
	// time a register changes it, on the client's goroutine before the box
	// hears of the change. It must not block.
	OnTags           func(tags map[string]string)
	Now              func() time.Time
	RegisterInterval time.Duration
	RotateInterval   time.Duration
	// AddrMemory is how long an observed address of one family is kept
	// without being observed again.
	AddrMemory time.Duration
}

// ReverseClient lets this device's paired helpers reach it as an inviter: it
// maps a TCP port for the VPN box's reverse portal and keeps a sealed route to
// it registered with lantern-cloud for each inviter pairing. It is safe for
// concurrent use.
type ReverseClient struct {
	cfg         ReverseConfig
	coverDomain string
	kick        chan struct{}
	netChanged  atomic.Bool

	mu     sync.Mutex
	cancel context.CancelFunc
	// done closes when the last started run has torn down.
	done chan struct{}

	// st is owned by the run goroutine; a run starts only after the previous
	// one's done closes.
	st reverseState
}

type observedAddr struct {
	ip netip.Addr
	at time.Time
}

type reverseState struct {
	fwd     portForwarder
	mapping *portforward.Mapping
	// lastPort is the internal port of the last mapping, tried again on a
	// remap so the portal keeps its listener.
	lastPort uint16
	creds    *reverseCreds
	credsAt  time.Time
	v4, v6   observedAddr
	tags     map[string]string
	// registered says lantern-cloud may hold routes this client sent.
	registered    bool
	registeredIDs []string
	// boxSet says the box may hold a portal this client configured.
	boxSet         bool
	portal         *samizdatreverse.PortalConfig
	portalCreds    *reverseCreds
	outbounds      []string
	outboundsKnown bool
	apiNotBefore   time.Time
	backoff        time.Duration
	warnedRouteCap bool
}

// NewReverseClient returns a stopped ReverseClient.
func NewReverseClient(cfg ReverseConfig) *ReverseClient {
	if cfg.NewForwarder == nil {
		cfg.NewForwarder = newQuietForwarder
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.RegisterInterval == 0 {
		cfg.RegisterInterval = defaultReverseRegisterInterval
	}
	if cfg.RotateInterval == 0 {
		cfg.RotateInterval = defaultReverseRotateInterval
	}
	if cfg.AddrMemory == 0 {
		cfg.AddrMemory = defaultReverseAddrMemory
	}
	return &ReverseClient{
		cfg:         cfg,
		coverDomain: pickReverseCoverDomain(),
		kick:        make(chan struct{}, 1),
	}
}

// Start starts serving in the background and returns at once; it does no
// network I/O itself. Starting a started client does nothing. It returns an
// error only when API, Keys or Box is nil.
func (c *ReverseClient) Start(context.Context) error {
	if c.cfg.API == nil || c.cfg.Keys == nil || c.cfg.Box == nil {
		return errors.New("peer: ReverseConfig needs API, Keys and Box")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	prev, done := c.done, make(chan struct{})
	c.cancel, c.done = cancel, done
	go c.run(ctx, prev, done)
	return nil
}

// Stop deregisters this device's routes, removes the portal and its reverse
// outbounds from the box, and unmaps the port, in that order. It waits for that
// teardown or for ctx, whichever ends first; the teardown has its own
// deadline and finishes either way.
func (c *ReverseClient) Stop(ctx context.Context) {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// NetworkChanged forgets the observed addresses, deregisters, rebinds the
// portal, maps the port again and registers at once.
func (c *ReverseClient) NetworkChanged() {
	c.netChanged.Store(true)
	c.wake()
}

// PairingsChanged registers at once for the current inviter pairings.
func (c *ReverseClient) PairingsChanged() { c.wake() }

func (c *ReverseClient) wake() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *ReverseClient) run(ctx context.Context, prev <-chan struct{}, done chan struct{}) {
	defer close(done)
	if prev != nil {
		<-prev
	}
	defer c.teardown()
	for {
		wait := c.cycle(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *ReverseClient) teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), peerCleanupTimeout)
	defer cancel()
	s := &c.st
	c.deregister(ctx, c.cfg.Now())
	c.clearBox()
	c.unmap(ctx)
	s.v4, s.v6 = observedAddr{}, observedAddr{}
	s.tags = nil
	s.creds, s.credsAt = nil, time.Time{}
}

// cycle does one round of work and returns how long to wait before the next.
func (c *ReverseClient) cycle(ctx context.Context) time.Duration {
	s := &c.st
	now := c.cfg.Now()
	gated := now.Before(s.apiNotBefore)
	if c.netChanged.Swap(false) {
		s.v4, s.v6 = observedAddr{}, observedAddr{}
		if !gated {
			c.deregister(ctx, now)
		}
		if s.boxSet {
			if err := c.cfg.Box.RebindReversePortal(); err != nil {
				slog.Warn("reverse: could not rebind the portal")
			}
		}
		c.unmap(ctx)
		gated = now.Before(s.apiNotBefore)
	}
	ids := c.cfg.Keys.IDs(string(helperticket.RoleInviter))
	if len(ids) == 0 {
		if !gated {
			c.deregister(ctx, now)
		}
		c.clearBox()
		c.unmap(ctx)
		s.tags = nil
		return c.untilNextCycle(now)
	}
	if gated {
		return s.apiNotBefore.Sub(now)
	}
	if len(ids) > maxReverseRoutes && !s.warnedRouteCap {
		s.warnedRouteCap = true
		slog.Info("reverse: more inviter pairings than routes allowed, registering a subset",
			"pairings", len(ids), "routes", maxReverseRoutes)
	}
	if len(ids) > maxReverseRoutes {
		ids = ids[:maxReverseRoutes]
	}
	for id := range s.tags {
		if !slices.Contains(ids, id) {
			delete(s.tags, id)
		}
	}
	s.expire(now, c.cfg.AddrMemory)
	// The empty register comes before any mapping, so a disabled feature
	// never opens a port.
	if s.mapping == nil || !s.hasAddr() {
		resp, err := c.cfg.API.Register(ctx, nil)
		if err != nil {
			return c.apiFailed(ctx, err, now)
		}
		s.backoff = 0
		s.observe(resp.ObservedIP, now)
		if !s.hasAddr() {
			c.withdraw(ctx, now)
			return c.untilNextCycle(now)
		}
	}
	if s.mapping == nil && !c.mapPort(ctx) {
		return c.cfg.RegisterInterval
	}
	if s.creds == nil || now.Sub(s.credsAt) >= c.cfg.RotateInterval {
		creds, err := newReverseCreds(c.coverDomain)
		if err != nil {
			slog.Error("reverse: could not make listener credentials", "err", err)
			return c.cfg.RegisterInterval
		}
		s.creds, s.credsAt = &creds, now
	}
	if slices.ContainsFunc(s.registeredIDs, func(id string) bool { return !slices.Contains(ids, id) }) {
		if c.deregister(ctx, now); now.Before(s.apiNotBefore) {
			return s.apiNotBefore.Sub(now)
		}
	}
	c.applyBox(ids, now)
	for range 1 + maxReverseReregisters {
		// Routes advertise the identity the running portal holds, which a
		// failed reconfigure leaves at the previous credentials.
		if s.portal == nil || s.portal.ListenPort != s.mapping.InternalPort {
			return c.cfg.RegisterInterval
		}
		blobs := c.seal(ids, now)
		if len(blobs) == 0 {
			return c.cfg.RegisterInterval
		}
		s.registered = true
		resp, err := c.cfg.API.Register(ctx, blobs)
		if err != nil {
			return c.apiFailed(ctx, err, now)
		}
		s.backoff = 0
		s.registeredIDs = s.registeredIDs[:0]
		for _, b := range blobs {
			s.registeredIDs = append(s.registeredIDs, b.PairingID)
		}
		if c.updateTags(blobs, resp.Tags) {
			c.applyBox(ids, now)
		}
		if !s.observe(resp.ObservedIP, now) {
			break
		}
	}
	return c.cfg.RegisterInterval
}

// untilNextCycle is the register interval, or longer while API calls are
// held off.
func (c *ReverseClient) untilNextCycle(now time.Time) time.Duration {
	if wait := c.st.apiNotBefore.Sub(now); wait > c.cfg.RegisterInterval {
		return wait
	}
	return c.cfg.RegisterInterval
}

func (c *ReverseClient) mapPort(ctx context.Context) bool {
	s := &c.st
	fwd, err := c.cfg.NewForwarder(ctx)
	if err != nil {
		// Gateway errors name local addresses.
		slog.Info("reverse: no port forwarding",
			"unsupported", errors.Is(err, portforward.ErrNoPortForwarding))
		return false
	}
	port := s.lastPort
	if port == 0 {
		port = pickInternalPort()
	}
	mapping, err := fwd.MapPort(ctx, portforward.TCP, port, reversePortDescription)
	if err != nil {
		slog.Info("reverse: could not map a port",
			"unsupported", errors.Is(err, portforward.ErrNoPortForwarding))
		cleanupCtx, cancel := context.WithTimeout(context.Background(), peerCleanupTimeout)
		_ = fwd.UnmapPort(cleanupCtx)
		cancel()
		s.lastPort = 0
		return false
	}
	// Renewal runs until UnmapPort, which outlives the run's ctx in teardown.
	fwd.StartRenewal(context.Background())
	s.fwd, s.mapping, s.lastPort = fwd, mapping, mapping.InternalPort
	return true
}

func (c *ReverseClient) unmap(ctx context.Context) {
	s := &c.st
	if s.fwd == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerCleanupTimeout)
	defer cancel()
	if err := s.fwd.UnmapPort(cleanupCtx); err != nil {
		slog.Info("reverse: could not unmap the port")
	}
	s.fwd, s.mapping = nil, nil
}

func (c *ReverseClient) deregister(ctx context.Context, now time.Time) {
	s := &c.st
	if !s.registered {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), peerCleanupTimeout)
	defer cancel()
	if err := c.cfg.API.Deregister(ctx); err != nil {
		c.apiFailed(ctx, err, now)
		return
	}
	s.registered = false
	s.registeredIDs = nil
}

// withdraw takes this device's routes and reverse outbounds down while it
// knows no address to reach it at, keeping the mapping and portal.
func (c *ReverseClient) withdraw(ctx context.Context, now time.Time) {
	c.deregister(ctx, now)
	c.setOutbounds(nil)
}

func (c *ReverseClient) clearBox() {
	s := &c.st
	if !s.boxSet {
		return
	}
	if err := c.cfg.Box.SetReversePortal(nil, ""); err != nil {
		slog.Warn("reverse: could not remove the portal")
		return
	}
	s.boxSet = false
	s.portal, s.portalCreds = nil, nil
	s.outbounds, s.outboundsKnown = nil, true
}

func (c *ReverseClient) apiFailed(ctx context.Context, err error, now time.Time) time.Duration {
	s := &c.st
	if ctx.Err() != nil {
		return c.cfg.RegisterInterval
	}
	var apiErr *helper.ReverseAPIError
	switch {
	case errors.Is(err, helper.ErrReverseDisabled):
		slog.Info("reverse: routes disabled by lantern-cloud")
		s.apiNotBefore = now.Add(reverseDisabledWait)
		// A disabled server serves no routes, and the rows it holds expire.
		s.registered, s.registeredIDs = false, nil
		c.clearBox()
		c.unmap(ctx)
		s.creds, s.credsAt = nil, time.Time{}
		return reverseDisabledWait
	case errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusTooManyRequests || apiErr.Status == http.StatusServiceUnavailable):
		s.backoff = min(2*max(s.backoff, c.cfg.RegisterInterval), maxReverseBackoff)
		slog.Info("reverse: lantern-cloud is busy, backing off", "wait", s.backoff)
		s.apiNotBefore = now.Add(s.backoff)
		return s.backoff
	}
	slog.Warn("reverse: lantern-cloud call failed", "err", err)
	return c.cfg.RegisterInterval
}

// seal returns a fresh blob for each pairing in ids, for the portal's
// credentials.
func (c *ReverseClient) seal(ids []string, now time.Time) []helper.ReverseBlob {
	s := &c.st
	route := reverse.Route{
		PublicKey:      s.portalCreds.PublicKey,
		CertSPKISHA256: s.portalCreds.SPKI,
		CoverDomain:    s.portalCreds.CoverDomain,
		IssuedAt:       now,
	}
	// The router forwards only IPv4, to the internal port; IPv6 reaches the
	// listener directly.
	if s.v4.ip.IsValid() {
		route.Addrs = append(route.Addrs, netip.AddrPortFrom(s.v4.ip, s.mapping.ExternalPort))
	}
	if s.v6.ip.IsValid() {
		route.Addrs = append(route.Addrs, netip.AddrPortFrom(s.v6.ip, s.mapping.InternalPort))
	}
	blobs := make([]helper.ReverseBlob, 0, len(ids))
	failed := 0
	for _, id := range ids {
		key, ok := c.cfg.Keys.Get(id)
		if !ok {
			continue
		}
		blob, err := reverse.Seal(key, id, route)
		if err != nil || len(blob) == 0 || len(blob) > reverse.MaxBlobLen || blob[0] != reverse.BlobVersion {
			failed++
			continue
		}
		blobs = append(blobs, helper.ReverseBlob{PairingID: id, Blob: blob, Version: reverse.BlobVersion})
	}
	if failed > 0 {
		slog.Error("reverse: could not seal routes", "failed", failed)
	}
	return blobs
}

// updateTags takes the tags of the registered pairings from a register
// response and reports whether any changed.
func (c *ReverseClient) updateTags(blobs []helper.ReverseBlob, tags map[string]string) bool {
	s := &c.st
	before := maps.Clone(s.tags)
	if s.tags == nil {
		s.tags = make(map[string]string)
	}
	for _, b := range blobs {
		if tag := tags[b.PairingID]; tag != "" {
			s.tags[b.PairingID] = tag
		} else {
			delete(s.tags, b.PairingID)
		}
	}
	if maps.Equal(before, s.tags) {
		return false
	}
	if c.cfg.OnTags != nil {
		c.cfg.OnTags(maps.Clone(s.tags))
	}
	return true
}

// applyBox brings the portal and reverse outbounds in line with the current
// credentials, port and tags, retrying whatever failed last time.
func (c *ReverseClient) applyBox(ids []string, now time.Time) {
	s := &c.st
	pairings := make(map[[8]byte]string)
	var tags []string
	for _, id := range ids {
		tag := lbA.ReverseHelperTag(s.tags[id])
		if tag == "" {
			continue
		}
		key, ok := c.cfg.Keys.Get(id)
		if !ok {
			continue
		}
		for _, sid := range reverse.AcceptedShortIDs(key, now) {
			pairings[sid] = tag
		}
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	tags = slices.Compact(tags)

	if s.portal == nil || s.portalCreds != s.creds || s.portal.ListenPort != s.mapping.InternalPort ||
		!maps.Equal(s.portal.Pairings, pairings) {
		cfg := &samizdatreverse.PortalConfig{
			ListenPort: s.mapping.InternalPort,
			PrivateKey: s.creds.PrivateKey[:],
			CertPEM:    s.creds.CertPEM,
			KeyPEM:     s.creds.KeyPEM,
			Pairings:   pairings,
		}
		s.boxSet = true
		if err := c.cfg.Box.SetReversePortal(cfg, c.coverDomain); err != nil {
			// The error can name the port or a tag.
			slog.Warn("reverse: could not configure the portal")
			return
		}
		s.portal, s.portalCreds = cfg, s.creds
	}
	c.setOutbounds(tags)
}

func (c *ReverseClient) setOutbounds(tags []string) {
	s := &c.st
	if s.outboundsKnown && slices.Equal(s.outbounds, tags) {
		return
	}
	if err := c.cfg.Box.SetReverseOutbounds(tags); err != nil {
		// The error can name a tag.
		slog.Warn("reverse: could not set the reverse outbounds")
		s.outboundsKnown = false
		return
	}
	s.outbounds, s.outboundsKnown = tags, true
}

func (s *reverseState) hasAddr() bool { return s.v4.ip.IsValid() || s.v6.ip.IsValid() }

func (s *reverseState) expire(now time.Time, memory time.Duration) {
	if s.v4.ip.IsValid() && now.Sub(s.v4.at) > memory {
		s.v4 = observedAddr{}
	}
	if s.v6.ip.IsValid() && now.Sub(s.v6.at) > memory {
		s.v6 = observedAddr{}
	}
}

// observe records ip for its family and reports whether it differs from the
// address held for that family.
func (s *reverseState) observe(ip string, now time.Time) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if addr.Zone() != "" {
		return false
	}
	held := &s.v6
	if addr.Is4() {
		held = &s.v4
	}
	changed := held.ip != addr
	*held = observedAddr{ip: addr, at: now}
	return changed
}
