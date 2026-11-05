package vpn

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	lbA "github.com/getlantern/lantern-box/adapter"
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

	require.NoError(t, c.SetReversePortal(testPortalConfig(t, rev)))
	portal := livePortal(t, tun)
	require.NotNil(t, portal)
	require.NotNil(t, portal.Addr(), "configured with a pairing, the portal listens")
	port := portal.Addr().(*net.TCPAddr).Port
	require.NoError(t, c.SetReverseOutbounds([]string{rev}))

	require.NoError(t, c.RebindReversePortal())
	require.NotNil(t, portal.Addr())
	assert.Equal(t, port, portal.Addr().(*net.TCPAddr).Port, "a rebind keeps the port")

	bad := testPortalConfig(t, rev)
	bad.PrivateKey = []byte{1, 2, 3}
	assert.Error(t, c.SetReversePortal(bad), "a configure error reaches the caller")

	require.NoError(t, c.SetReversePortal(nil))
	assert.Nil(t, livePortal(t, tun))
	assert.Nil(t, portal.Addr())
	requireTornDown(t, tun, rev)
	assert.Nil(t, tun.personalProxiesGroup())
	assert.Empty(t, c.ReverseOutbounds())
	require.NoError(t, c.RebindReversePortal(), "nothing to rebind")
	require.NoError(t, c.SetReversePortal(nil), "nothing to remove")
}

// State set while no tunnel runs is kept and applied to the next tunnel.
func TestReverseStateAppliesToTheNextTunnel(t *testing.T) {
	c := NewVPNClient(t.TempDir(), rlog.NoOpLogger(), nil)
	rev := lbA.ReverseHelperTag(testHelper1)
	require.NoError(t, c.SetReversePortal(testPortalConfig(t, rev)))
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
