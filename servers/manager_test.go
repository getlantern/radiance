package servers

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	C "github.com/getlantern/common"
	box "github.com/getlantern/lantern-box"

	_ "github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrivateServerIntegration(t *testing.T) {
	manager := testManager(t)
	manager.httpClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	srv := newLanternServerManagerMock()
	defer srv.Close()
	parsedURL, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(parsedURL.Port())

	t.Run("convert a token into a custom server", func(t *testing.T) {
		require.NoError(t, manager.AddPrivateServer("s1", parsedURL.Hostname(), port, "rootToken", C.ServerLocation{}, false))
		require.Contains(t, manager.optsMap, "s1", "server should be added to the manager")
	})

	t.Run("invite user", func(t *testing.T) {
		inviteToken, err := manager.InviteToPrivateServer(parsedURL.Hostname(), port, "rootToken", "invite1")
		assert.NoError(t, err)
		assert.NotEmpty(t, inviteToken)

		require.NoError(t, manager.AddPrivateServer("s2", parsedURL.Hostname(), port, inviteToken, C.ServerLocation{}, true))
		require.Contains(t, manager.optsMap, "s2", "server should be added for the invited user")

		t.Run("revoke user access", func(t *testing.T) {
			delete(manager.optsMap, "s1")
			require.NoError(t, manager.RevokePrivateServerInvite(parsedURL.Hostname(), port, "rootToken", "invite1"))
			// trying to access again with the same token should fail
			assert.Error(t, manager.AddPrivateServer("s1", parsedURL.Hostname(), port, inviteToken, C.ServerLocation{}, true))
			assert.NotContains(t, manager.optsMap, "s1", "server should not be added after revoking invite")
		})
	})

}

type lanternServerManagerMock struct {
	users      map[string]string
	testConfig string
}

func newLanternServerManagerMock() *httptest.Server {
	testConfig := `
{
	"outbounds": [
		{
			"tag": "testing-out",
			"type": "shadowsocks",
			"server": "127.0.0.1",
			"server_port": 1080,
			"method": "chacha20-ietf-poly1305",
			"password": "<PASSWORD>",
		}
	]
}
`
	srv := httptest.NewUnstartedServer(&lanternServerManagerMock{
		testConfig: testConfig,
		users: map[string]string{
			"rootToken": testConfig,
		},
	})
	srv.StartTLS()
	return srv
}

func (s *lanternServerManagerMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if r.URL.Path == "/api/v1/connect-config" {
		if s.users[token] != "" {
			_, _ = w.Write([]byte(s.users[token]))
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/share-link/") {
		if token != "rootToken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inviteName := strings.TrimPrefix(r.URL.Path, "/api/v1/share-link/")
		s.users[inviteName] = s.testConfig
		_, _ = w.Write([]byte(fmt.Sprintf(`{"token":"%s"}`, inviteName)))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/revoke/") {
		if token != "rootToken" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inviteName := strings.TrimPrefix(r.URL.Path, "/api/v1/revoke/")
		delete(s.users, inviteName)
		_, _ = w.Write([]byte("OK"))
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func TestAddServersByJSON(t *testing.T) {
	t.Run("valid config", func(t *testing.T) {
		testConfig := []byte(`
{
	"outbounds": [
		{
			"tag": "out",
			"type": "shadowsocks",
			"server": "127.0.0.1",
			"server_port": 1080,
			"method": "chacha20-ietf-poly1305",
			"password": "<PASSWORD>",
		}
	]
}`)
		options, err := json.UnmarshalExtendedContext[Options](box.BaseContext(), testConfig)
		require.NoError(t, err, "failed to unmarshal test config")
		want := Server{
			Group:   SGUser,
			Tag:     "out",
			Type:    "shadowsocks",
			Options: options.Outbounds[0],
		}
		m := testManager(t)
		require.NoError(t, m.AddServersByJSON(t.Context(), testConfig))
		got, exists := m.GetServerByTag("out")
		assert.True(t, exists, "server was not added")
		assert.Equal(t, want, got, "added server does not match expected configuration")
	})
	t.Run("empty config", func(t *testing.T) {
		m := testManager(t)
		assert.Error(t, m.AddServersByJSON(t.Context(), []byte("{}")))
		assert.Empty(t, m.optsMap, "no servers should have been added")
	})
}

func TestAddServersByURL(t *testing.T) {
	urls := []string{
		"vless://uuid@host:443?encryption=none&security=tls&type=ws&host=example.com&path=/vless#VLESS+over+WS+with+TLS",
		"trojan://password@host:443?security=tls&sni=example.com#Trojan+with+TLS",
	}
	t.Run("valid urls", func(t *testing.T) {
		m := testManager(t)
		require.NoError(t, m.AddServersByURL(t.Context(), urls, false))
		_, exists := m.GetServerByTag("VLESS+over+WS+with+TLS")
		assert.True(t, exists, "VLESS server should be added")
		_, exists = m.GetServerByTag("Trojan+with+TLS")
		assert.True(t, exists, "Trojan server should be added")
	})
	t.Run("skip certificate", func(t *testing.T) {
		m := testManager(t)
		require.NoError(t, m.AddServersByURL(t.Context(), urls, true))
		server, exists := m.GetServerByTag("Trojan+with+TLS")
		require.True(t, exists, "Trojan server should be added")

		options := server.Options.(option.Outbound).Options
		require.IsType(t, &option.TrojanOutboundOptions{}, options)
		trojanOpts := options.(*option.TrojanOutboundOptions)
		require.NotNil(t, trojanOpts.TLS)
		assert.True(t, trojanOpts.TLS.Insecure, "TLS.Insecure should be true")
	})
	t.Run("empty urls", func(t *testing.T) {
		m := testManager(t)
		assert.Error(t, m.AddServersByURL(t.Context(), []string{}, false))
		assert.Empty(t, m.optsMap, "no servers should have been added")
	})
}

func TestRetryableHTTPClient(t *testing.T) {
	cli := retryableHTTPClient(log.NoOpLogger()).StandardClient()
	request, err := http.NewRequest(http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
	require.NoError(t, err)
	resp, err := cli.Do(request)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func testManager(t *testing.T) *Manager {
	return &Manager{
		servers: Servers{
			SGLantern: Options{
				Outbounds:   make([]option.Outbound, 0),
				Endpoints:   make([]option.Endpoint, 0),
				Locations:   make(map[string]C.ServerLocation),
				Credentials: make(map[string]ServerCredentials),
			},
			SGUser: Options{
				Outbounds:   make([]option.Outbound, 0),
				Endpoints:   make([]option.Endpoint, 0),
				Locations:   make(map[string]C.ServerLocation),
				Credentials: make(map[string]ServerCredentials),
			},
		},
		optsMap:     map[string]Server{},
		serversFile: filepath.Join(t.TempDir(), internal.ServersFileName),
		logger:      log.NoOpLogger(),
	}
}
