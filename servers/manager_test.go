package servers

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	C "github.com/getlantern/common"

	"github.com/sagernet/sing-box/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
)

func TestPrivateServerIntegration(t *testing.T) {
	dataPath := t.TempDir()
	manager := &Manager{
		servers: Servers{
			SGLantern: Options{
				Outbounds: make([]option.Outbound, 0),
				Endpoints: make([]option.Endpoint, 0),
				Locations: make(map[string]C.ServerLocation),
			},
			SGUser: Options{
				Outbounds: make([]option.Outbound, 0),
				Endpoints: make([]option.Endpoint, 0),
				Locations: make(map[string]C.ServerLocation),
			},
		},
		optsMaps: map[ServerGroup]map[string]any{
			SGLantern: make(map[string]any),
			SGUser:    make(map[string]any),
		},
		serversFile:      filepath.Join(dataPath, common.ServersFileName),
		fingerprintsFile: filepath.Join(dataPath, trustFingerprintFileName),
		log:              slog.Default(),
	}

	srv := newLanternServerManagerMock()
	defer srv.Close()
	parsedURL, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(parsedURL.Port())

	trustingCallback := func(ip string, details []CertDetail) *CertDetail {
		return &details[0]
	}
	t.Run("convert a token into a custom server", func(t *testing.T) {
		require.NoError(t, manager.AddPrivateServer("s1", parsedURL.Hostname(), port, "rootToken", trustingCallback))
		require.Contains(t, manager.optsMaps[SGUser], "s1", "server should be added to the manager")
	})

	t.Run("invite a user", func(t *testing.T) {
		inviteToken, err := manager.InviteToPrivateServer(parsedURL.Hostname(), port, "rootToken", "invite1")
		assert.NoError(t, err)
		assert.NotEmpty(t, inviteToken)

		require.NoError(t, manager.AddPrivateServer("s2", parsedURL.Hostname(), port, inviteToken, trustingCallback))
		require.Contains(t, manager.optsMaps[SGUser], "s2", "server should be added for the invited user")

		t.Run("revoke user access", func(t *testing.T) {
			delete(manager.optsMaps[SGUser], "s1")
			require.NoError(t, manager.RevokePrivateServerInvite(parsedURL.Hostname(), port, "rootToken", "invite1"))
			// trying to access again with the same token should fail
			assert.Error(t, manager.AddPrivateServer("s1", parsedURL.Hostname(), port, inviteToken, trustingCallback))
			assert.NotContains(t, manager.optsMaps[SGUser], "s1", "server should not be added after revoking invite")
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
	"inbounds": [
	],
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
