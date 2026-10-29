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
