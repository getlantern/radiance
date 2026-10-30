package servers

import (
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/log"
)

func socksServer(tag string, isLantern bool) *Server {
	return &Server{Tag: tag, Type: "socks", IsLantern: isLantern,
		Options: option.Outbound{Tag: tag, Type: "socks", Options: &option.SOCKSOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "203.0.113.7", ServerPort: 1080},
		}}}
}

// User servers may not take a tag the tunnel reserves, such as the personal
// proxies group's; every import refuses one before storing anything.
func TestUserServersCannotUseReservedTags(t *testing.T) {
	mgr, err := NewManager(t.TempDir(), log.NoOpLogger())
	require.NoError(t, err)

	_, err = mgr.AddServersByJSON(t.Context(), []byte(
		`{"outbounds":[{"type":"socks","tag":"ok-1","server":"203.0.113.7","server_port":1080},`+
			`{"type":"socks","tag":"personal-proxies","server":"203.0.113.7","server_port":1080}]}`))
	require.ErrorIs(t, err, ErrReservedTag)
	assert.Empty(t, mgr.AllServers(), "a refused import stores none of its servers")

	_, err = mgr.AddServersByURL(t.Context(), []string{"trojan://password@example.com:443?security=tls&sni=example.com#personal-proxies"}, false)
	require.ErrorIs(t, err, ErrReservedTag)

	for _, tag := range ReservedTags() {
		require.ErrorIs(t, mgr.AddServers(ServerList{Servers: []*Server{socksServer(tag, false)}}, false), ErrReservedTag, tag)
	}
	// Refused before contacting the server manager at that address.
	err = mgr.AddPrivateServer("personal-proxies", "203.0.113.7", 1, "token", C.ServerLocation{}, true)
	require.ErrorIs(t, err, ErrReservedTag)
	assert.Empty(t, mgr.AllServers())

	// A Lantern config may carry the built-in outbounds.
	require.NoError(t, mgr.AddServers(ServerList{Servers: []*Server{socksServer("direct", true)}}, false))
}

// The tunnel creates a server's outbound under the tag the outbound carries,
// so a user server's embedded tag is set to the server's own: a server named
// "mine" carrying an outbound tagged "auto" becomes "mine" throughout, and
// cannot replace the tunnel's own group; a reserved server tag is refused.
func TestUserServerTagIsTheEffectiveTag(t *testing.T) {
	mgr, err := NewManager(t.TempDir(), log.NoOpLogger())
	require.NoError(t, err)

	for _, tag := range ReservedTags() {
		hidden := socksServer("mine-"+tag, false)
		hidden.Options = socksServer(tag, false).Options
		list := ServerList{Servers: []*Server{hidden}}
		require.NoError(t, mgr.AddServers(list, false), tag)
		assert.Equal(t, []string{"mine-" + tag}, outboundTags(list.Outbounds()), "what reaches the tunnel")
		stored, ok := mgr.GetServerByTag("mine-" + tag)
		require.True(t, ok)
		assert.Equal(t, "mine-"+tag, stored.Options.(option.Outbound).Tag)
		_, ok = mgr.GetServerByTag(tag)
		assert.False(t, ok)
	}

	endpoint := &Server{Tag: "mine-ep", Type: "wireguard", Options: option.Endpoint{Tag: "manual", Type: "wireguard"}}
	normalizeUserTag(endpoint)
	assert.Equal(t, "mine-ep", endpoint.Options.(option.Endpoint).Tag)
	lantern := &Server{Tag: "lantern-1", IsLantern: true, Options: option.Outbound{Tag: "lantern-x"}}
	normalizeUserTag(lantern)
	assert.Equal(t, "lantern-x", lantern.Options.(option.Outbound).Tag, "config servers are left alone")

	reserved := socksServer("auto", false)
	reserved.Options = socksServer("fine", false).Options
	require.ErrorIs(t, mgr.AddServers(ServerList{Servers: []*Server{reserved}}, false), ErrReservedTag,
		"a reserved server tag is still refused")
}

func outboundTags(outs []option.Outbound) []string {
	var tags []string
	for _, o := range outs {
		tags = append(tags, o.Tag)
	}
	return tags
}
