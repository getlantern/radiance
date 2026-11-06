package peer

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	"github.com/getlantern/lantern-box/portforward"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"
	"github.com/getlantern/samizdat"

	"github.com/getlantern/broflake/common/helperticket"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/helper"
	"github.com/getlantern/radiance/helper/reverse"
)

type revLog struct {
	mu     sync.Mutex
	events []string
}

func (l *revLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *revLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ev := l.events
	l.events = nil
	return ev
}

type revClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *revClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *revClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type revKeys struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func (k *revKeys) Get(id string) ([]byte, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, ok := k.keys[id]
	return slices.Clone(key), ok
}

func (k *revKeys) IDs(role string) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if role != string(helperticket.RoleInviter) {
		return nil
	}
	var ids []string
	for id := range k.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (k *revKeys) set(id string) []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.keys == nil {
		k.keys = map[string][]byte{}
	}
	sum := sha256.Sum256([]byte("key-" + id))
	k.keys[id] = sum[:]
	return slices.Clone(sum[:])
}

func (k *revKeys) remove(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.keys, id)
}

type revBox struct {
	mu           sync.Mutex
	log          *revLog
	portals      []*samizdatreverse.PortalConfig
	masquerades  []string
	outbounds    [][]string
	rebinds      int
	portalErr    error
	outboundsErr error
}

func (b *revBox) SetReversePortal(cfg *samizdatreverse.PortalConfig, masqueradeDomain string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cfg == nil {
		b.log.add("clear-portal")
	} else {
		b.log.add("portal")
	}
	b.portals = append(b.portals, cfg)
	b.masquerades = append(b.masquerades, masqueradeDomain)
	return b.portalErr
}

func (b *revBox) SetReverseOutbounds(tags []string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log.add(fmt.Sprintf("outbounds:%d", len(tags)))
	b.outbounds = append(b.outbounds, slices.Clone(tags))
	return b.outboundsErr
}

func (b *revBox) RebindReversePortal() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log.add("rebind")
	b.rebinds++
	return nil
}

func (b *revBox) lastPortal() *samizdatreverse.PortalConfig {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.portals) == 0 {
		return nil
	}
	return b.portals[len(b.portals)-1]
}

func (b *revBox) lastOutbounds() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.outbounds) == 0 {
		return nil
	}
	return b.outbounds[len(b.outbounds)-1]
}

func (b *revBox) setErrs(portal, outbounds error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.portalErr, b.outboundsErr = portal, outbounds
}

// revForwarder is a fakeForwarder whose router hands out an external port
// different from the internal one.
type revForwarder struct {
	*fakeForwarder
	log      *revLog
	mu       sync.Mutex
	internal []uint16
}

const revExternalOffset = 7

func (f *revForwarder) MapPort(ctx context.Context, protocol string, internalPort uint16, desc string) (*portforward.Mapping, error) {
	m, err := f.fakeForwarder.MapPort(ctx, protocol, internalPort, desc)
	if err != nil {
		f.log.add("map-failed")
		return nil, err
	}
	f.log.add("map")
	f.mu.Lock()
	f.internal = append(f.internal, internalPort)
	f.mu.Unlock()
	mapped := *m
	mapped.ExternalPort = internalPort + revExternalOffset
	return &mapped, nil
}

func (f *revForwarder) UnmapPort(ctx context.Context) error {
	f.log.add("unmap")
	return f.fakeForwarder.UnmapPort(ctx)
}

func (f *revForwarder) mappedPorts() []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.internal)
}

type revResponse struct {
	status     int
	observedIP string
	// tags overrides the default of a tag for every named pairing.
	tags map[string]string
}

type revServer struct {
	srv *httptest.Server
	log *revLog

	mu          sync.Mutex
	registers   [][]helper.ReverseBlob
	deregisters int
	// next answers register calls in order; once used up, every call gets
	// fallback.
	next        []revResponse
	fallback    revResponse
	deregStatus int
	block       chan struct{}
}

func revTag(pairingID string) string {
	sum := sha256.Sum256([]byte(pairingID))
	return lbA.HelperTagPrefix + hex.EncodeToString(sum[:6])
}

func newRevServer(t *testing.T, log *revLog) *revServer {
	s := &revServer{log: log, fallback: revResponse{observedIP: "198.51.100.7"}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		block := s.block
		s.mu.Unlock()
		if block != nil {
			<-block
		}
		switch r.URL.Path {
		case "/v1/helper/reverse/register":
			var req struct {
				Routes []helper.ReverseBlob `json:"routes"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			s.mu.Lock()
			s.registers = append(s.registers, req.Routes)
			resp := s.fallback
			if len(s.next) > 0 {
				resp, s.next = s.next[0], s.next[1:]
			}
			s.mu.Unlock()
			log.add(fmt.Sprintf("register:%d", len(req.Routes)))
			if resp.status != 0 && resp.status != http.StatusOK {
				w.WriteHeader(resp.status)
				return
			}
			tags := resp.tags
			if tags == nil {
				tags = map[string]string{}
				for _, b := range req.Routes {
					tags[b.PairingID] = revTag(b.PairingID)
				}
			}
			_ = json.NewEncoder(w).Encode(helper.ReverseRegisterResponse{ObservedIP: resp.observedIP, Tags: tags})
		case "/v1/helper/reverse/deregister":
			s.mu.Lock()
			s.deregisters++
			status := s.deregStatus
			s.mu.Unlock()
			log.add("deregister")
			if status == 0 {
				status = http.StatusOK
			}
			w.WriteHeader(status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *revServer) queue(rs ...revResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = append(s.next, rs...)
}

func (s *revServer) setFallback(r revResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = r
}

func (s *revServer) lastRegister() []helper.ReverseBlob {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.registers) == 0 {
		return nil
	}
	return s.registers[len(s.registers)-1]
}

func (s *revServer) registerCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.registers)
}

type revEnv struct {
	c     *ReverseClient
	log   *revLog
	clock *revClock
	keys  *revKeys
	box   *revBox
	fwd   *revForwarder
	srv   *revServer
	// forwarders counts NewForwarder calls.
	forwarders int
}

func newRevEnv(t *testing.T) *revEnv {
	t.Helper()
	log := &revLog{}
	e := &revEnv{
		log:   log,
		clock: &revClock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)},
		keys:  &revKeys{},
		box:   &revBox{log: log},
		fwd:   &revForwarder{fakeForwarder: &fakeForwarder{}, log: log},
		srv:   newRevServer(t, log),
	}
	e.c = NewReverseClient(ReverseConfig{
		API:  helper.NewReverseAPI(e.srv.srv.Client(), e.srv.srv.URL+"/v1", "test-device"),
		Keys: e.keys,
		Box:  e.box,
		NewForwarder: func(context.Context) (portForwarder, error) {
			e.forwarders++
			return e.fwd, nil
		},
		Now: e.clock.Now,
	})
	return e
}

func (e *revEnv) cycle(t *testing.T) time.Duration {
	t.Helper()
	return e.c.cycle(context.Background())
}

func openBlob(t *testing.T, key []byte, b helper.ReverseBlob, now time.Time) reverse.Route {
	t.Helper()
	assert.Equal(t, reverse.BlobVersion, b.Version)
	r, err := reverse.Open(key, b.PairingID, b.Blob, now)
	require.NoError(t, err)
	return r
}

func x25519Public(t *testing.T, priv []byte) [32]byte {
	t.Helper()
	k, err := ecdh.X25519().NewPrivateKey(priv)
	require.NoError(t, err)
	var pub [32]byte
	copy(pub[:], k.PublicKey().Bytes())
	return pub
}

func TestReverseIdleWithoutKeys(t *testing.T) {
	e := newRevEnv(t)
	assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
	assert.Zero(t, e.forwarders)
	assert.Empty(t, e.log.take())
}

func TestReverseRegistersSealedRoute(t *testing.T) {
	e := newRevEnv(t)
	key := e.keys.set("p1")
	e.srv.queue(revResponse{observedIP: "198.51.100.7"})

	assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
	assert.Equal(t, []string{"register:0", "map", "portal", "outbounds:0", "register:1", "portal", "outbounds:1"}, e.log.take())

	ports := e.fwd.mappedPorts()
	require.Len(t, ports, 1)
	internal := ports[0]
	portal := e.box.lastPortal()
	require.NotNil(t, portal)
	assert.Equal(t, internal, portal.ListenPort)

	blobs := e.srv.lastRegister()
	require.Len(t, blobs, 1)
	r := openBlob(t, key, blobs[0], e.clock.Now())
	assert.Equal(t, []netip.AddrPort{netip.MustParseAddrPort(fmt.Sprintf("198.51.100.7:%d", internal+revExternalOffset))}, r.Addrs)
	assert.Equal(t, x25519Public(t, portal.PrivateKey), r.PublicKey)
	assert.Equal(t, e.c.coverDomain, r.CoverDomain)
	assert.Equal(t, e.c.coverDomain, e.box.masquerades[len(e.box.masquerades)-1])
	block, _ := pem.Decode(portal.CertPEM)
	require.NotNil(t, block)
	spki, err := samizdat.SPKISHA256(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, spki, r.CertSPKISHA256)

	wantTag := lbA.ReverseHelperTag(revTag("p1"))
	assert.Equal(t, []string{wantTag}, e.box.lastOutbounds())
	want := map[[8]byte]string{}
	for _, sid := range reverse.AcceptedShortIDs(key, e.clock.Now()) {
		want[sid] = wantTag
	}
	assert.Equal(t, want, portal.Pairings)

	// A steady tick reseals with a fresh issued_at and leaves the box alone.
	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"register:1"}, e.log.take())
	r2 := openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	assert.Equal(t, e.clock.Now().Unix(), r2.IssuedAt.Unix())
}

func TestReverseObservedIPChangeRegistersAgain(t *testing.T) {
	e := newRevEnv(t)
	key := e.keys.set("p1")
	e.cycle(t)
	e.log.take()

	e.clock.Advance(defaultReverseRegisterInterval)
	e.srv.setFallback(revResponse{observedIP: "203.0.113.5"})
	e.cycle(t)
	assert.Equal(t, []string{"register:1", "register:1"}, e.log.take())
	r := openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	require.Len(t, r.Addrs, 1)
	assert.Equal(t, netip.MustParseAddr("203.0.113.5"), r.Addrs[0].Addr())
}

func TestReverseKeepsOneAddressPerFamily(t *testing.T) {
	e := newRevEnv(t)
	key := e.keys.set("p1")
	e.srv.queue(revResponse{observedIP: "198.51.100.7"}, revResponse{observedIP: "2001:db8::7"})
	e.srv.setFallback(revResponse{observedIP: "2001:db8::7"})

	e.cycle(t)
	assert.Equal(t, []string{"register:0", "map", "portal", "outbounds:0", "register:1", "portal", "outbounds:1", "register:1"}, e.log.take())
	internal := e.fwd.mappedPorts()[0]
	r := openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	assert.ElementsMatch(t, []netip.AddrPort{
		netip.MustParseAddrPort(fmt.Sprintf("198.51.100.7:%d", internal+revExternalOffset)),
		netip.MustParseAddrPort(fmt.Sprintf("[2001:db8::7]:%d", internal)),
	}, r.Addrs)

	// Only the IPv6 address is seen again, so the IPv4 one expires an hour
	// after it was last seen.
	e.clock.Advance(30 * time.Minute)
	e.cycle(t)
	r = openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	assert.Len(t, r.Addrs, 2)

	e.clock.Advance(31 * time.Minute)
	e.cycle(t)
	r = openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	assert.Equal(t, []netip.AddrPort{netip.MustParseAddrPort(fmt.Sprintf("[2001:db8::7]:%d", internal))}, r.Addrs)
}

func TestReverseNoAddressDeregisters(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.srv.setFallback(revResponse{observedIP: ""})

	e.cycle(t)
	assert.Equal(t, []string{"register:0", "outbounds:0"}, e.log.take())
	assert.False(t, e.fwd.wasMapped(), "no port is mapped without an address to advertise")

	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"register:0"}, e.log.take())

	// An address learned later brings the route back.
	e.srv.setFallback(revResponse{observedIP: "198.51.100.7"})
	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"register:0", "map", "portal", "register:1", "portal", "outbounds:1"}, e.log.take())

	// Losing every address withdraws the registered route and the outbounds.
	e.c.NetworkChanged()
	e.srv.setFallback(revResponse{observedIP: ""})
	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"deregister", "rebind", "unmap", "register:0", "outbounds:0"}, e.log.take())
}

func TestReverseRotationKeepsPortAndCoverDomain(t *testing.T) {
	e := newRevEnv(t)
	key := e.keys.set("p1")
	e.cycle(t)
	first := e.box.lastPortal()
	r1 := openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())

	e.clock.Advance(defaultReverseRotateInterval)
	e.log.take()
	e.cycle(t)
	assert.Equal(t, []string{"portal", "register:1"}, e.log.take())
	second := e.box.lastPortal()
	r2 := openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())

	assert.Equal(t, first.ListenPort, second.ListenPort)
	assert.NotEqual(t, first.PrivateKey, second.PrivateKey)
	assert.NotEqual(t, r1.PublicKey, r2.PublicKey)
	assert.Equal(t, x25519Public(t, second.PrivateKey), r2.PublicKey)
	assert.Equal(t, r1.CoverDomain, r2.CoverDomain)
	assert.Equal(t, r1.Addrs, r2.Addrs)
	assert.Equal(t, 1, len(e.fwd.mappedPorts()))
}

func TestReverseNetworkChangedRemaps(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.cycle(t)
	e.log.take()

	e.c.NetworkChanged()
	select {
	case <-e.c.kick:
	default:
		t.Fatal("NetworkChanged did not ask for a cycle")
	}
	e.cycle(t)
	assert.Equal(t, []string{"deregister", "rebind", "unmap", "register:0", "map", "register:1"}, e.log.take())
	ports := e.fwd.mappedPorts()
	require.Len(t, ports, 2)
	assert.Equal(t, ports[0], ports[1], "the portal keeps its port across a remap")
}

func TestReversePairingsChangedDeregistersRemoved(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	k2 := e.keys.set("p2")
	e.cycle(t)
	e.log.take()

	e.keys.remove("p1")
	e.c.PairingsChanged()
	e.cycle(t)
	assert.Equal(t, []string{"portal", "outbounds:1", "deregister", "register:1"}, e.log.take())
	blobs := e.srv.lastRegister()
	require.Len(t, blobs, 1)
	assert.Equal(t, "p2", blobs[0].PairingID)
	openBlob(t, k2, blobs[0], e.clock.Now())
	assert.Equal(t, []string{lbA.ReverseHelperTag(revTag("p2"))}, e.box.lastOutbounds())

	// The last pairing gone: clear the box, deregister and unmap.
	e.keys.remove("p2")
	e.cycle(t)
	assert.Equal(t, []string{"clear-portal", "deregister", "unmap"}, e.log.take())
	e.cycle(t)
	assert.Empty(t, e.log.take())
}

func TestReverseCapsRoutes(t *testing.T) {
	e := newRevEnv(t)
	var ids []string
	for i := range 25 {
		id := fmt.Sprintf("p%02d", i)
		e.keys.set(id)
		ids = append(ids, id)
	}
	e.cycle(t)
	blobs := e.srv.lastRegister()
	require.Len(t, blobs, maxReverseRoutes)
	var got []string
	for _, b := range blobs {
		got = append(got, b.PairingID)
		assert.LessOrEqual(t, len(b.Blob), reverse.MaxBlobLen)
	}
	assert.Equal(t, ids[:maxReverseRoutes], got)
	assert.Len(t, e.box.lastOutbounds(), maxReverseRoutes)
}

func TestReverseSkipsInvalidTags(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.keys.set("p2")
	e.srv.setFallback(revResponse{observedIP: "198.51.100.7", tags: map[string]string{"p1": revTag("p1"), "p2": "not-a-helper-tag"}})
	e.cycle(t)
	assert.Equal(t, []string{lbA.ReverseHelperTag(revTag("p1"))}, e.box.lastOutbounds())
	for _, tag := range e.box.lastPortal().Pairings {
		assert.Equal(t, lbA.ReverseHelperTag(revTag("p1")), tag)
	}
}

func TestReverseDisabledIdlesAnHour(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.srv.setFallback(revResponse{status: http.StatusNotFound})

	assert.Equal(t, reverseDisabledWait, e.cycle(t))
	assert.Equal(t, []string{"register:0"}, e.log.take(), "a disabled feature maps no port")

	e.clock.Advance(30 * time.Minute)
	e.c.PairingsChanged()
	assert.Equal(t, 30*time.Minute, e.cycle(t))
	assert.Empty(t, e.log.take())

	e.srv.setFallback(revResponse{observedIP: "198.51.100.7"})
	e.clock.Advance(30 * time.Minute)
	e.cycle(t)
	assert.Contains(t, e.log.take(), "register:1")
}

func TestReverseBacksOffOnOverload(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e.srv.setFallback(revResponse{status: status})
			want := defaultReverseRegisterInterval
			for range 6 {
				want = min(2*want, maxReverseBackoff)
				assert.Equal(t, want, e.cycle(t))
				e.clock.Advance(want)
			}
			assert.Equal(t, maxReverseBackoff, want)

			e.srv.setFallback(revResponse{observedIP: "198.51.100.7"})
			assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
			e.clock.Advance(defaultReverseRegisterInterval)
		})
	}
}

func TestReverseOtherErrorsRetryNextTick(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.srv.setFallback(revResponse{status: http.StatusInternalServerError})
	assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
	assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
	assert.Equal(t, 2, e.srv.registerCount())
}

func TestReverseMapFailureRetriesNextTick(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.fwd.mapErr = errors.New("no gateway at 192.168.1.1")
	assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
	assert.Equal(t, []string{"register:0", "map-failed", "unmap"}, e.log.take())
	assert.Empty(t, e.srv.lastRegister(), "no route is registered without a mapping")

	e.fwd.mapErr = nil
	e.cycle(t)
	assert.Contains(t, e.log.take(), "register:1")
}

func TestReverseBoxErrorsRetryNextTick(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.box.setErrs(errors.New("bind failed"), nil)
	assert.Equal(t, defaultReverseRegisterInterval, e.cycle(t))
	assert.Equal(t, []string{"register:0", "map", "portal"}, e.log.take(), "no route while no portal serves it")

	e.box.setErrs(nil, errors.New("outbounds failed"))
	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"portal", "outbounds:0", "register:1", "portal", "outbounds:1"}, e.log.take())

	e.box.setErrs(nil, nil)
	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"outbounds:1", "register:1"}, e.log.take())

	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"register:1"}, e.log.take())
}

func TestReverseStartReturnsAtOnceAndStopTearsDownInOrder(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	gate := make(chan struct{})
	e.srv.mu.Lock()
	e.srv.block = gate
	e.srv.mu.Unlock()

	start := time.Now()
	require.NoError(t, e.c.Start(context.Background()))
	require.NoError(t, e.c.Start(context.Background()), "Start is idempotent")
	assert.Less(t, time.Since(start), time.Second)

	e.srv.mu.Lock()
	e.srv.block = nil
	e.srv.mu.Unlock()
	close(gate)
	require.Eventually(t, func() bool {
		return slices.Contains(e.box.lastOutbounds(), lbA.ReverseHelperTag(revTag("p1")))
	}, 5*time.Second, 10*time.Millisecond)
	e.log.take()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.c.Stop(ctx)
	assert.Equal(t, []string{"deregister", "clear-portal", "unmap"}, e.log.take())
	assert.True(t, e.fwd.renewStopped)

	// A restart serves again.
	require.NoError(t, e.c.Start(context.Background()))
	require.Eventually(t, func() bool {
		return slices.Contains(e.log.take(), "register:1")
	}, 5*time.Second, 10*time.Millisecond)
	e.c.Stop(ctx)
}

func TestReverseKickRunsACycle(t *testing.T) {
	e := newRevEnv(t)
	require.NoError(t, e.c.Start(context.Background()))
	defer e.c.Stop(context.Background())

	e.keys.set("p1")
	e.c.PairingsChanged()
	require.Eventually(t, func() bool { return e.srv.registerCount() >= 2 }, 5*time.Second, 10*time.Millisecond)
}

func TestReverseCredsAreConsistent(t *testing.T) {
	creds, err := newReverseCreds("www.example.com")
	require.NoError(t, err)
	assert.Equal(t, x25519Public(t, creds.PrivateKey[:]), creds.PublicKey)

	block, _ := pem.Decode(creds.CertPEM)
	require.NotNil(t, block)
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, []string{"www.example.com"}, cert.DNSNames)
	assert.True(t, cert.NotBefore.Before(time.Now().Add(-23*time.Hour)))
	assert.True(t, cert.NotAfter.After(time.Now().Add(364*24*time.Hour)))
	spki, err := samizdat.SPKISHA256(block.Bytes)
	require.NoError(t, err)
	assert.Equal(t, spki, creds.SPKI)

	keyBlock, _ := pem.Decode(creds.KeyPEM)
	require.NotNil(t, keyBlock)
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	require.NoError(t, err)
	assert.True(t, key.PublicKey.Equal(cert.PublicKey))

	other, err := newReverseCreds("www.example.com")
	require.NoError(t, err)
	assert.NotEqual(t, creds.PrivateKey, other.PrivateKey)
}

func TestReverseCoverDomainsFit(t *testing.T) {
	for _, d := range reverseCoverDomains {
		assert.LessOrEqual(t, len(d), reverse.MaxDomainLen)
		assert.False(t, strings.ContainsAny(d, " /:"))
	}
}

func TestReverseFailedRotationKeepsAdvertisingTheRunningPortal(t *testing.T) {
	e := newRevEnv(t)
	key := e.keys.set("p1")
	e.cycle(t)
	running := e.box.lastPortal()

	e.box.setErrs(errors.New("bind failed"), nil)
	e.clock.Advance(defaultReverseRotateInterval)
	e.log.take()
	e.cycle(t)
	assert.Equal(t, []string{"portal", "register:1"}, e.log.take())
	r := openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	assert.Equal(t, x25519Public(t, running.PrivateKey), r.PublicKey)

	e.box.setErrs(nil, nil)
	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Equal(t, []string{"portal", "register:1"}, e.log.take())
	r = openBlob(t, key, e.srv.lastRegister()[0], e.clock.Now())
	assert.Equal(t, x25519Public(t, e.box.lastPortal().PrivateKey), r.PublicKey)
	assert.NotEqual(t, x25519Public(t, running.PrivateKey), r.PublicKey)
}

func TestReverseDisabledTearsDown(t *testing.T) {
	e := newRevEnv(t)
	e.keys.set("p1")
	e.cycle(t)
	e.log.take()

	e.srv.setFallback(revResponse{status: http.StatusNotFound})
	e.clock.Advance(defaultReverseRegisterInterval)
	assert.Equal(t, reverseDisabledWait, e.cycle(t))
	assert.Equal(t, []string{"register:1", "clear-portal", "unmap"}, e.log.take())

	e.c.NetworkChanged()
	e.clock.Advance(59 * time.Minute)
	e.cycle(t)
	assert.Empty(t, e.log.take())

	// Stop has nothing left to withdraw.
	e.c.teardown()
	assert.Empty(t, e.log.take())
}

func TestReverseDeregisterErrorsBackOff(t *testing.T) {
	for _, tc := range []struct {
		status int
		wait   time.Duration
	}{
		{http.StatusNotFound, reverseDisabledWait},
		{http.StatusTooManyRequests, 2 * defaultReverseRegisterInterval},
		{http.StatusServiceUnavailable, 2 * defaultReverseRegisterInterval},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			e := newRevEnv(t)
			e.keys.set("p1")
			e.cycle(t)
			e.keys.remove("p1")
			e.srv.mu.Lock()
			e.srv.deregStatus = tc.status
			e.srv.mu.Unlock()
			e.log.take()

			assert.Equal(t, tc.wait, e.cycle(t))
			assert.Equal(t, []string{"clear-portal", "deregister", "unmap"}, e.log.take())
			for range 3 {
				e.clock.Advance(tc.wait / 4)
				e.cycle(t)
			}
			assert.Empty(t, e.log.take(), "no calls while held off")

			e.clock.Advance(tc.wait)
			e.cycle(t)
			if tc.status == http.StatusNotFound {
				assert.Empty(t, e.log.take(), "a disabled server keeps no route to withdraw")
			} else {
				assert.Equal(t, []string{"deregister"}, e.log.take())
			}
		})
	}
}

func TestReverseNeverLogsTheManualPort(t *testing.T) {
	require.NoError(t, settings.InitSettings(t.TempDir()))
	t.Cleanup(settings.Reset)
	require.NoError(t, settings.Set(settings.PeerManualPortKey, 41234))
	var buf strings.Builder
	var bufMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		bufMu.Lock()
		defer bufMu.Unlock()
		return buf.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newRevEnv(t)
	e.c.cfg.NewForwarder = nil
	e.c = NewReverseClient(ReverseConfig{API: e.c.cfg.API, Keys: e.keys, Box: e.box, Now: e.clock.Now})
	e.keys.set("p1")
	e.cycle(t)
	e.c.NetworkChanged()
	e.cycle(t)
	require.Equal(t, uint16(41234), e.box.lastPortal().ListenPort)

	bufMu.Lock()
	defer bufMu.Unlock()
	assert.Contains(t, buf.String(), "manual port forward")
	assert.NotContains(t, buf.String(), "41234")
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestReverseReportsTagsBeforeTheBox(t *testing.T) {
	e := newRevEnv(t)
	var got []map[string]string
	var outboundsAtReport [][]string
	e.c.cfg.OnTags = func(tags map[string]string) {
		got = append(got, tags)
		outboundsAtReport = append(outboundsAtReport, e.box.lastOutbounds())
	}
	e.keys.set("p1")
	e.srv.setFallback(revResponse{observedIP: "198.51.100.7", tags: map[string]string{"p1": revTag("p1")}})
	e.cycle(t)
	require.Equal(t, []map[string]string{{"p1": revTag("p1")}}, got)
	assert.Empty(t, outboundsAtReport[0])
	assert.Equal(t, []string{lbA.ReverseHelperTag(revTag("p1"))}, e.box.lastOutbounds())

	e.clock.Advance(defaultReverseRegisterInterval)
	e.cycle(t)
	assert.Len(t, got, 1, "an unchanged map is not reported again")
}

// A revoked pairing leaves the portal and reverse outbounds on the next
// cycle even while lantern-cloud fails or is backed off from; the rest stay.
func TestReverseDropsARevokedPairingWhileTheAPIFails(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newRevEnv(t)
			e.keys.set("p1")
			k2 := e.keys.set("p2")
			e.cycle(t)
			tag1, tag2 := lbA.ReverseHelperTag(revTag("p1")), lbA.ReverseHelperTag(revTag("p2"))
			require.ElementsMatch(t, []string{tag1, tag2}, e.box.lastOutbounds())

			e.srv.setFallback(revResponse{status: status})
			e.clock.Advance(defaultReverseRegisterInterval)
			e.cycle(t)
			e.log.take()
			registers := e.srv.registerCount()

			e.keys.remove("p1")
			e.c.PairingsChanged()
			e.cycle(t)
			assert.Equal(t, []string{tag2}, e.box.lastOutbounds())
			want := map[[8]byte]string{}
			for _, sid := range reverse.AcceptedShortIDs(k2, e.clock.Now()) {
				want[sid] = tag2
			}
			assert.Equal(t, want, e.box.lastPortal().Pairings)
			if status == http.StatusServiceUnavailable {
				assert.Equal(t, registers, e.srv.registerCount(), "held off")
			}
		})
	}
}

func TestReverseNetworkChangedMarksThePublicIPStale(t *testing.T) {
	common.SetPublicIP("198.51.100.9")
	t.Cleanup(func() { common.SetPublicIP("") })
	e := newRevEnv(t)
	require.False(t, common.PublicIPStale())
	e.c.NetworkChanged()
	assert.True(t, common.PublicIPStale())
}
