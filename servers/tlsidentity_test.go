package servers

import (
	"os"
	"testing"
	"time"

	C "github.com/getlantern/common"
	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportRequiresTLSVerification(t *testing.T) {
	for _, uri := range []string{
		"vless://uuid@example.com:443?security=tls&allowInsecure=1#proxy",
		"trojan://password@example.com:443?security=tls&allowInsecure=1#proxy",
		"hysteria2://password@example.com:443?insecure=1#proxy",
	} {
		t.Run(uri[:8], func(t *testing.T) {
			for _, skip := range []bool{false, true} {
				mgr := testManager(t)
				list, err := mgr.AddServersByURL(t.Context(), []string{uri}, skip)
				require.NoError(t, err)
				require.Len(t, list.Servers, 1)
				assertVerifiedTLS(t, list.Servers[0])
				stored, exists := mgr.GetServerByTag(list.Servers[0].Tag)
				require.True(t, exists)
				assertVerifiedTLS(t, stored)
			}
		})
	}
	mgr := testManager(t)
	list, err := mgr.AddServersByJSON(t.Context(), []byte(`{"outbounds":[{
		"type":"hysteria2","tag":"proxy","server":"example.com","server_port":443,
		"tls":{"enabled":true,"insecure":true}
	}]}`))
	require.NoError(t, err)
	require.Len(t, list.Servers, 1)
	assertVerifiedTLS(t, list.Servers[0])
}

func TestAddServersPreservesMetadataAndInput(t *testing.T) {
	opts := &option.Hysteria2OutboundOptions{
		ServerOptions: option.ServerOptions{Server: "example.com", ServerPort: 443},
		OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{TLS: &option.OutboundTLSOptions{
			Enabled: true, Insecure: true, ServerName: "example.com", Certificate: []string{"trusted certificate"},
		}},
	}
	srv := &Server{
		Tag: "proxy", Type: "hysteria2", IsLantern: true,
		Options:          option.Outbound{Tag: "proxy", Type: "hysteria2", Options: opts},
		Location:         C.ServerLocation{CountryCode: "DE", City: "Frankfurt"},
		Credentials:      &ServerCredentials{AccessToken: "token", Port: 443, IsJoined: true},
		SelectionHistory: &SelectionHistory{ConsecutiveFailures: 2, UpdatedAt: time.Unix(1, 0).UTC()},
	}
	mgr := testManager(t)
	require.NoError(t, mgr.AddServers(ServerList{Servers: []*Server{srv}}, false))
	assert.True(t, opts.TLS.Insecure)
	stored, exists := mgr.GetServerByTag(srv.Tag)
	require.True(t, exists)
	assertVerifiedTLS(t, stored)
	want := srv.Clone()
	want.Options = stored.Options
	assert.Equal(t, want, stored)
	assert.NotSame(t, srv.Credentials, stored.Credentials)
	assert.NotSame(t, srv.SelectionHistory, stored.SelectionHistory)
	storedTLS := stored.Options.(option.Outbound).Options.(*option.Hysteria2OutboundOptions).TLS
	assert.Equal(t, opts.TLS.ServerName, storedTLS.ServerName)
	assert.Equal(t, opts.TLS.Certificate, storedTLS.Certificate)
	assert.NotSame(t, opts.TLS, storedTLS)
	raw, err := os.ReadFile(mgr.serversFile)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"insecure":true`)
}

func TestLoadServersRequiresTLSVerification(t *testing.T) {
	outbound := `{"type":"hysteria2","tag":"proxy","server":"example.com","server_port":443,"tls":{"enabled":true,"insecure":true}}`
	for name, data := range map[string]string{
		"current": `[{"tag":"proxy","type":"hysteria2","isLantern":true,"outbound":` + outbound + `}]`,
		"legacy":  `{"lantern":{"outbounds":[` + outbound + `]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			mgr := testManager(t)
			require.NoError(t, os.WriteFile(mgr.serversFile, []byte(data), 0o600))
			require.NoError(t, mgr.loadServers())
			srv, exists := mgr.GetServerByTag("proxy")
			require.True(t, exists)
			assert.True(t, srv.IsLantern)
			assertVerifiedTLS(t, srv)
			require.NoError(t, mgr.saveServers())
			raw, err := os.ReadFile(mgr.serversFile)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), `"insecure":true`)
		})
	}
}

func assertVerifiedTLS(t *testing.T, srv *Server) {
	t.Helper()
	tls := srv.Options.(option.Outbound).Options.(option.OutboundTLSOptionsWrapper).TakeOutboundTLSOptions()
	require.NotNil(t, tls)
	assert.True(t, tls.Enabled)
	assert.False(t, tls.Insecure)
}
