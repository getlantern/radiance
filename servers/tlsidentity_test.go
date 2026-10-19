package servers

import (
	"os"
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const insecureHTTP = `{"tag":"private","type":"http","server":"example.com","server_port":443,"tls":{"enabled":true,"insecure":true}}`

func requireVerifiedTLS(t *testing.T, server *Server) {
	t.Helper()
	out := server.Options.(option.Outbound)
	tls := out.Options.(option.OutboundTLSOptionsWrapper).TakeOutboundTLSOptions()
	require.NotNil(t, tls)
	assert.False(t, tls.Insecure)
}

func TestURLFlagsCannotDisableTLSIdentity(t *testing.T) {
	for _, url := range []string{
		"trojan://password@example.com:443?allowInsecure=1#private",
		"vless://uuid@example.com:443?security=tls&allowInsecure=true#private",
		"hysteria://example.com:443?insecure=1#private",
		"hy2://password@example.com:443?insecure=true#private",
	} {
		for _, legacySkip := range []bool{false, true} {
			manager := testManager(t)
			list, err := manager.AddServersByURL(t.Context(), []string{url}, legacySkip)
			require.NoError(t, err)
			require.Len(t, list.Servers, 1)
			requireVerifiedTLS(t, list.Servers[0])
			stored, found := manager.GetServerByTag("private")
			require.True(t, found)
			requireVerifiedTLS(t, stored)
		}
	}
}

func TestJSONAndSavedServersRequireTLSIdentity(t *testing.T) {
	manager := testManager(t)
	list, err := manager.AddServersByJSON(t.Context(), []byte(`{"outbounds":[`+insecureHTTP+`]}`))
	require.NoError(t, err)
	requireVerifiedTLS(t, list.Servers[0])
	data, err := os.ReadFile(manager.serversFile)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"insecure":true`)

	for _, format := range []string{
		`[{"tag":"private","type":"http","outbound":` + insecureHTTP + `}]`,
		`{"user":{"outbounds":[` + insecureHTTP + `]}}`,
	} {
		manager := testManager(t)
		require.NoError(t, os.WriteFile(manager.serversFile, []byte(format), 0o600))
		require.NoError(t, manager.loadServers())
		stored, found := manager.GetServerByTag("private")
		require.True(t, found)
		requireVerifiedTLS(t, stored)
	}
}

func TestAddServersRequiresTLSIdentityWithoutMutatingInput(t *testing.T) {
	manager := testManager(t)
	options := &option.HTTPOutboundOptions{OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{
		TLS: &option.OutboundTLSOptions{Enabled: true, Insecure: true},
	}}
	server := &Server{Tag: "private", Type: "http", Options: option.Outbound{Tag: "private", Type: "http", Options: options}}
	require.NoError(t, manager.AddServers(ServerList{Servers: []*Server{server}}, false))
	stored, found := manager.GetServerByTag("private")
	require.True(t, found)
	requireVerifiedTLS(t, stored)
	assert.True(t, options.TLS.Insecure)
}
