package vpn

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"

	rlog "github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/servers"
)

func newReverseTestClient(t *testing.T, tun *tunnel) *VPNClient {
	t.Helper()
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	c.tunnel = tun
	return c
}

func testPortalConfig(t *testing.T, reverseTags ...string) *samizdatreverse.PortalConfig {
	t.Helper()
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	pairings := make(map[[8]byte]string, len(reverseTags))
	for i, tag := range reverseTags {
		pairings[[8]byte{byte(i + 1)}] = tag
	}
	return &samizdatreverse.PortalConfig{
		PrivateKey: priv.Bytes(),
		CertPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		KeyPEM:     pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		Pairings:   pairings,
	}
}

// livePortal returns the tunnel's portal inbound, or nil when it has none.
func livePortal(t *testing.T, tun *tunnel) *samizdatreverse.Portal {
	t.Helper()
	in, ok := service.FromContext[adapter.InboundManager](tun.ctx).Get(reversePortalTag)
	if !ok {
		return nil
	}
	p, ok := in.(*samizdatreverse.Portal)
	require.True(t, ok)
	return p
}

// requireTornDown waits for the group manager's queued teardown of tag.
func requireTornDown(t *testing.T, tun *tunnel, tag string) {
	t.Helper()
	require.Eventually(t, func() bool { return tun.liveOutbound(tag) == nil },
		10*time.Second, 50*time.Millisecond)
}

func TestReverseOutboundsJoinTheFriendsGroups(t *testing.T) {
	tun, _ := startPersonalProxyTestTunnel(t, friendA)
	c := newReverseTestClient(t, tun)
	revA, revB := lbA.ReverseHelperTag(friendA), lbA.ReverseHelperTag(friendB)

	require.NoError(t, c.SetReverseOutbounds([]string{revA, revB}))
	assert.ElementsMatch(t, []string{friendA, consumer, revA, revB}, personalProxiesMembers(tun))
	for _, groupTag := range []string{ManualSelectTag, AutoSelectTag} {
		assert.Subset(t, selectorMembers(t, tun, groupTag), []string{revA, revB}, groupTag)
	}

	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendA)))
	assert.ElementsMatch(t, []string{friendA, consumer, revA}, personalProxyMembers(tun))

	// friendB has neither a direct outbound nor a ticket: its reverse
	// outbound alone makes it selectable.
	require.NoError(t, tun.selectOutbound(PersonalProxySelection(friendB)))
	assert.Equal(t, []string{revB}, personalProxyMembers(tun))

	require.NoError(t, c.SetReverseOutbounds([]string{revA}))
	assert.Nil(t, tun.personalProxyGroup(), "friendB's group lost its last member")
	assert.Equal(t, PersonalProxiesTag, selectedInManual(t, tun))
}

// Helper reconciliation and config updates are driven by the server manager,
// which never holds reverse outbounds, so they must leave them alone.
func TestHelperReconciliationKeepsReverseOutbounds(t *testing.T) {
	tun := startPersonalProxiesTestTunnel(t)
	c := newReverseTestClient(t, tun)
	rev := lbA.ReverseHelperTag(testHelper1)
	require.NoError(t, c.SetReverseOutbounds([]string{rev}))
	require.NoError(t, c.ReconcileHelpers(servers.ServerList{Servers: []*servers.Server{directServer(testHelper2)}}))
	require.NoError(t, c.ReconcileHelpers(servers.ServerList{}))
	require.NoError(t, c.UpdateOutbounds(servers.ServerList{Servers: []*servers.Server{directServer("lantern-1")}}))
	require.NoError(t, c.RemoveOutbounds([]string{"lantern-2"}))
	for _, groupTag := range []string{ManualSelectTag, AutoSelectTag} {
		assert.Contains(t, selectorMembers(t, tun, groupTag), rev, groupTag)
	}
	assert.Equal(t, []string{rev}, personalProxiesMembers(tun))
}

func TestReverseOutboundsSwapWithoutEmptyingThePersonalProxiesGroup(t *testing.T) {
	tun := startPersonalProxiesTestTunnel(t)
	c := newReverseTestClient(t, tun)
	revA, revB := lbA.ReverseHelperTag(testHelper1), lbA.ReverseHelperTag(testHelper2)

	require.NoError(t, c.SetReverseOutbounds([]string{revA}))
	group := tun.personalProxiesGroup()
	require.NotNil(t, group)
	require.NoError(t, tun.selectOutbound(PersonalProxiesTag))

	require.NoError(t, c.SetReverseOutbounds([]string{revB}))
	assert.Same(t, group, tun.personalProxiesGroup(), "the group was never withdrawn")
	assert.Equal(t, []string{revB}, personalProxiesMembers(tun))
	assert.Equal(t, PersonalProxiesTag, selectedInManual(t, tun))
	for _, groupTag := range []string{ManualSelectTag, AutoSelectTag} {
		assert.NotContains(t, selectorMembers(t, tun, groupTag), revA, groupTag)
	}

	require.NoError(t, c.SetReverseOutbounds(nil))
	assert.Nil(t, tun.personalProxiesGroup())
	requireTornDown(t, tun, revB)
	assert.Equal(t, AutoSelectTag, tun.clashServer.Mode())
}

func TestSetReverseOutboundsRefusesOtherTags(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	for _, tag := range []string{"lantern-1", testHelper1, PersonalProxiesTag, ""} {
		assert.Error(t, c.SetReverseOutbounds([]string{tag}), tag)
	}
	assert.Empty(t, c.ReverseOutbounds())
}

func TestReversePortalFollowsItsConfig(t *testing.T) {
	tun := startPersonalProxiesTestTunnel(t)
	c := newReverseTestClient(t, tun)
	rev := lbA.ReverseHelperTag(testHelper1)

	require.NoError(t, c.SetReversePortal(testPortalConfig(t, rev), "a.example.com"))
	portal := livePortal(t, tun)
	require.NotNil(t, portal)
	require.NotNil(t, portal.Addr(), "configured with a pairing, the portal listens")
	port := portal.Addr().(*net.TCPAddr).Port
	require.NoError(t, c.SetReverseOutbounds([]string{rev}))

	require.NoError(t, c.RebindReversePortal())
	require.NotNil(t, portal.Addr())
	assert.Equal(t, port, portal.Addr().(*net.TCPAddr).Port, "a rebind keeps the port")

	require.NoError(t, c.SetReversePortal(testPortalConfig(t, rev), "a.example.com"))
	assert.Same(t, portal, livePortal(t, tun), "the same masquerade keeps the portal")
	require.NoError(t, c.SetReversePortal(testPortalConfig(t, rev), "b.example.com"))
	replaced := livePortal(t, tun)
	require.NotNil(t, replaced)
	assert.NotSame(t, portal, replaced, "a new masquerade replaces the portal")
	assert.Nil(t, portal.Addr(), "the replaced portal is closed")
	require.NotNil(t, replaced.Addr())
	portal = replaced

	bad := testPortalConfig(t, rev)
	bad.PrivateKey = []byte{1, 2, 3}
	assert.Error(t, c.SetReversePortal(bad, "a.example.com"), "a configure error reaches the caller")

	require.NoError(t, c.SetReversePortal(nil, ""))
	assert.Nil(t, livePortal(t, tun))
	assert.Nil(t, portal.Addr())
	requireTornDown(t, tun, rev)
	assert.Nil(t, tun.personalProxiesGroup())
	assert.Empty(t, c.ReverseOutbounds())
	require.NoError(t, c.RebindReversePortal(), "nothing to rebind")
	require.NoError(t, c.SetReversePortal(nil, ""), "nothing to remove")
}

// State set while no tunnel runs is kept and applied to the next tunnel.
func TestReverseStateAppliesToTheNextTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	rev := lbA.ReverseHelperTag(testHelper1)
	require.NoError(t, c.SetReversePortal(testPortalConfig(t, rev), "a.example.com"))
	require.NoError(t, c.SetReverseOutbounds([]string{rev}))
	require.NoError(t, c.RebindReversePortal())

	for range 2 {
		tun := startPersonalProxiesTestTunnel(t)
		c.mu.Lock()
		c.tunnel = tun
		require.NoError(t, c.restoreReverseLocked())
		c.mu.Unlock()
		portal := livePortal(t, tun)
		require.NotNil(t, portal)
		assert.NotNil(t, portal.Addr())
		assert.NotNil(t, tun.liveOutbound(rev))
		assert.Equal(t, []string{rev}, personalProxiesMembers(tun))
		require.NoError(t, tun.close())
		assert.Nil(t, portal.Addr(), "the portal closes with its tunnel")
		c.mu.Lock()
		c.tunnel = nil
		c.mu.Unlock()
	}
}

// A portal whose restore could not bind on a new tunnel reports no listener,
// and the kept config brings it up once the port is free.
func TestReversePortalListeningAfterAFailedRestore(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.False(t, c.ReversePortalListening(), "no tunnel")
	occupied, err := net.Listen("tcp", "[::]:0")
	require.NoError(t, err)
	cfg := testPortalConfig(t, lbA.ReverseHelperTag(testHelper1))
	cfg.ListenPort = uint16(occupied.Addr().(*net.TCPAddr).Port)
	require.NoError(t, c.SetReversePortal(cfg, "a.example.com"))

	tun := startPersonalProxiesTestTunnel(t)
	c.mu.Lock()
	c.tunnel = tun
	assert.Error(t, c.restoreReverseLocked())
	c.mu.Unlock()
	assert.False(t, c.ReversePortalListening())

	require.NoError(t, occupied.Close())
	require.NoError(t, c.SetReversePortal(cfg, "a.example.com"))
	assert.True(t, c.ReversePortalListening())
	require.NoError(t, c.SetReversePortal(nil, ""))
	assert.False(t, c.ReversePortalListening())
}

// A tunnel starts with the kept reverse outbounds and portal in its options,
// so a selection whose only members are reverse outbounds starts on them.
func TestBuildOptionsStartsReverseOnlySelections(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	revA, revB := lbA.ReverseHelperTag(friendA), lbA.ReverseHelperTag(friendB)
	require.NoError(t, c.SetReversePortal(testPortalConfig(t, revA, revB), "cover.example.com"))
	require.NoError(t, c.SetReverseOutbounds([]string{revA, revB}))
	build := func(t *testing.T, initial string) (O.Options, *lbO.MutableSelectorOutboundOptions) {
		t.Helper()
		options, _ := testBoxOptions(t)
		c.mu.Lock()
		boxOptions := c.withReverse(c.withPersonalProxyRooms(BoxOptions{
			BasePath: t.TempDir(), Options: options, InitialServer: initial,
		}))
		c.mu.Unlock()
		opts, err := buildOptions(boxOptions)
		require.NoError(t, err)
		return opts, findOutbound(opts.Outbounds, ManualSelectTag).Options.(*lbO.MutableSelectorOutboundOptions)
	}
	groupMembers := func(opts O.Options, tag string) []string {
		group := findOutbound(opts.Outbounds, tag)
		require.NotNil(t, group, tag)
		return group.Options.(*lbO.MutableURLTestOutboundOptions).Outbounds
	}

	opts, manual := build(t, PersonalProxiesTag)
	assert.Equal(t, PersonalProxiesTag, manual.Outbounds[0])
	assert.ElementsMatch(t, []string{revA, revB}, groupMembers(opts, PersonalProxiesTag))
	auto := findOutbound(opts.Outbounds, AutoSelectTag).Options.(*lbO.MutableAutoSelectOutboundOptions)
	assert.Subset(t, auto.Outbounds, []string{revA, revB})
	var portal *O.Inbound
	for i := range opts.Inbounds {
		if opts.Inbounds[i].Tag == reversePortalTag {
			portal = &opts.Inbounds[i]
		}
	}
	require.NotNil(t, portal, "the portal starts with the tunnel")
	assert.Equal(t, lbC.TypeSamizdatPortal, portal.Type)
	assert.Equal(t, "cover.example.com", portal.Options.(*lbO.SamizdatPortalInboundOptions).MasqueradeDomain)

	opts, manual = build(t, PersonalProxySelection(friendB))
	assert.Equal(t, PersonalProxyTag, manual.Outbounds[0])
	assert.Equal(t, []string{revB}, groupMembers(opts, PersonalProxyTag))

	require.NoError(t, c.SetReversePortal(nil, ""))
	opts, _ = build(t, "")
	assert.Nil(t, findOutbound(opts.Outbounds, revA))
	assert.Nil(t, findOutbound(opts.Outbounds, PersonalProxiesTag))
	for _, in := range opts.Inbounds {
		assert.NotEqual(t, reversePortalTag, in.Tag)
	}
}

func TestLogsNeverNameReverseTags(t *testing.T) {
	rev := lbA.ReverseHelperTag(testHelper1)
	assert.Equal(t, []string{"lantern-1", testHelper1, "reverse"}, loggableTags([]string{"lantern-1", testHelper1, rev}))
	assert.NotContains(t, fmt.Sprint(loggableError(rev, errors.New("outbound/x["+rev+"]: failed"))), rev)
}

func TestPhysicalInterfaceControlNeedsATunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	assert.Nil(t, c.PhysicalInterfaceControl())

	tun := startPersonalProxiesTestTunnel(t)
	c.mu.Lock()
	c.tunnel = tun
	c.mu.Unlock()
	t.Cleanup(func() { _ = tun.close() })
	assert.NotPanics(t, func() { _ = c.PhysicalInterfaceControl() })
}
