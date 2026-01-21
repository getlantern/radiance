package servers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
		serversFile: filepath.Join(dataPath, common.ServersFileName),
	}

	srv := newLanternServerManagerMock()
	defer srv.Close()
	parsedURL, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(parsedURL.Port())
	trustingClient := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	t.Run("convert a token into a custom server", func(t *testing.T) {
		require.NoError(t, manager.AddPrivateServer("s1", parsedURL.Hostname(), port, "rootToken", &trustingClient))
		require.Contains(t, manager.optsMaps[SGUser], "s1", "server should be added to the manager")
	})

	t.Run("invite a user", func(t *testing.T) {
		inviteToken, err := manager.InviteToPrivateServer(parsedURL.Hostname(), port, "rootToken", "invite1", &trustingClient)
		assert.NoError(t, err)
		assert.NotEmpty(t, inviteToken)

		require.NoError(t, manager.AddPrivateServer("s2", parsedURL.Hostname(), port, inviteToken, &trustingClient))
		require.Contains(t, manager.optsMaps[SGUser], "s2", "server should be added for the invited user")

		t.Run("revoke user access", func(t *testing.T) {
			delete(manager.optsMaps[SGUser], "s1")
			require.NoError(t, manager.RevokePrivateServerInvite(parsedURL.Hostname(), port, "rootToken", "invite1", &trustingClient))
			// trying to access again with the same token should fail
			assert.Error(t, manager.AddPrivateServer("s1", parsedURL.Hostname(), port, inviteToken, &trustingClient))
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

func TestAddServerWithSingBoxJSON(t *testing.T) {
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
		serversFile: filepath.Join(dataPath, common.ServersFileName),
	}

	ctx := context.Background()
	jsonConfig := `
	{
		"outbounds": [
			{
               "type": "shadowsocks",
               "tag": "ss-out",
               "server": "127.0.0.1",
               "server_port": 8388,
               "method": "chacha20-ietf-poly1305",
               "password": "randompasswordwith24char",
               "network": "tcp"
            }
		]
	}`

	t.Run("adding server with a sing-box json config should work", func(t *testing.T) {
		require.NoError(t, manager.AddServerWithSingboxJSON(ctx, []byte(jsonConfig)))
	})
	t.Run("using a empty config should return an error", func(t *testing.T) {
		require.Error(t, manager.AddServerWithSingboxJSON(ctx, []byte{}))
	})
	t.Run("providing a json that doesn't have any endpoints or outbounds should return a error", func(t *testing.T) {
		require.Error(t, manager.AddServerWithSingboxJSON(ctx, json.RawMessage("{}")))
	})
}

func TestAddServerBasedOnURLs(t *testing.T) {
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
		serversFile: filepath.Join(dataPath, common.ServersFileName),
	}
	ctx := context.Background()
	after := func() {
		manager.RemoveServer("VLESS+over+WS+with+TLS")
		manager.RemoveServer("Trojan+with+TLS")
		manager.RemoveServer("SpecialName")
	}

	urls := strings.Join([]string{
		"vless://uuid@host:443?encryption=none&security=tls&type=ws&host=example.com&path=/vless#VLESS+over+WS+with+TLS",
		"trojan://password@host:443?security=tls&sni=example.com#Trojan+with+TLS",
	}, "\n")
	t.Run("adding server based on URLs should work", func(t *testing.T) {
		require.NoError(t, manager.AddServerBasedOnURLs(ctx, urls, false, ""))
		assert.Contains(t, manager.optsMaps[SGUser], "VLESS+over+WS+with+TLS")
		assert.Contains(t, manager.optsMaps[SGUser], "Trojan+with+TLS")
		after()
	})

	t.Run("using empty URLs should return an error", func(t *testing.T) {
		require.Error(t, manager.AddServerBasedOnURLs(ctx, "", false, ""))
	})

	t.Run("skip certificate verification option works", func(t *testing.T) {
		require.NoError(t, manager.AddServerBasedOnURLs(ctx, urls, true, ""))
		opts, isOutbound := manager.optsMaps[SGUser]["Trojan+with+TLS"].(option.Outbound)
		require.True(t, isOutbound)
		trojanSettings, ok := opts.Options.(*option.TrojanOutboundOptions)
		require.True(t, ok)
		require.NotNil(t, trojanSettings)
		require.NotNil(t, trojanSettings.TLS)
		assert.True(t, trojanSettings.OutboundTLSOptionsContainer.TLS.Insecure, trojanSettings.OutboundTLSOptionsContainer.TLS)
		after()
	})

	url := "vless://uuid@host:443?encryption=none&security=tls&type=ws&host=example.com&path=/vless#VLESS+over+WS+with+TLS"
	t.Run("adding single URL should work", func(t *testing.T) {
		require.NoError(t, manager.AddServerBasedOnURLs(ctx, url, false, "SpecialName"))
		assert.Contains(t, manager.optsMaps[SGUser], "SpecialName")
		assert.NotContains(t, manager.optsMaps[SGUser], "VLESS+over+WS+with+TLS")

		require.NoError(t, manager.AddServerBasedOnURLs(ctx, url, false, ""))
		assert.Contains(t, manager.optsMaps[SGUser], "VLESS+over+WS+with+TLS")
		assert.Contains(t, manager.optsMaps[SGUser], "SpecialName")
		after()
	})
}
func TestServers(t *testing.T) {
	dataPath := t.TempDir()
	manager := &Manager{
		servers: Servers{
			SGLantern: Options{
				Outbounds: []option.Outbound{
					{Tag: "lantern-out", Type: "shadowsocks"},
				},
				Endpoints: []option.Endpoint{
					{Tag: "lantern-ep", Type: "shadowsocks"},
				},
				Locations: map[string]C.ServerLocation{
					"lantern-out": {City: "New York", Country: "US"},
				},
			},
			SGUser: Options{
				Outbounds: []option.Outbound{
					{Tag: "user-out", Type: "trojan"},
				},
				Endpoints: []option.Endpoint{
					{Tag: "user-ep", Type: "vless"},
				},
				Locations: map[string]C.ServerLocation{
					"user-out": {City: "London", Country: "GB"},
				},
			},
		},
		optsMaps: map[ServerGroup]map[string]any{
			SGLantern: {
				"lantern-out": option.Outbound{Tag: "lantern-out", Type: "shadowsocks"},
				"lantern-ep":  option.Endpoint{Tag: "lantern-ep", Type: "shadowsocks"},
			},
			SGUser: {
				"user-out": option.Outbound{Tag: "user-out", Type: "trojan"},
				"user-ep":  option.Endpoint{Tag: "user-ep", Type: "vless"},
			},
		},
		serversFile: filepath.Join(dataPath, common.ServersFileName),
	}

	t.Run("returns copy of servers", func(t *testing.T) {
		servers := manager.Servers()

		require.NotNil(t, servers)
		require.Contains(t, servers, SGLantern)
		require.Contains(t, servers, SGUser)

		assert.Len(t, servers[SGLantern].Outbounds, 1)
		assert.Len(t, servers[SGLantern].Endpoints, 1)
		assert.Equal(t, "lantern-out", servers[SGLantern].Outbounds[0].Tag)
		assert.Equal(t, "lantern-ep", servers[SGLantern].Endpoints[0].Tag)

		assert.Len(t, servers[SGUser].Outbounds, 1)
		assert.Len(t, servers[SGUser].Endpoints, 1)
		assert.Equal(t, "user-out", servers[SGUser].Outbounds[0].Tag)
		assert.Equal(t, "user-ep", servers[SGUser].Endpoints[0].Tag)

		assert.Equal(t, "New York", servers[SGLantern].Locations["lantern-out"].City)
		assert.Equal(t, "London", servers[SGUser].Locations["user-out"].City)
	})

	t.Run("modifications to returned copy don't affect original", func(t *testing.T) {
		servers := manager.Servers()
		assert.Len(t, servers[SGLantern].Outbounds, 1)
		assert.Len(t, servers[SGUser].Endpoints, 1)

		// Modify the copy
		servers[SGLantern].Outbounds[0].Tag = "modified-out"

		// Original should remain unchanged
		originalServers := manager.Servers()
		assert.NotEqual(t, originalServers[SGLantern].Outbounds[0].Tag, "modified-out")
	})

	t.Run("handles empty servers", func(t *testing.T) {
		emptyManager := &Manager{
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
			serversFile: filepath.Join(dataPath, common.ServersFileName),
		}

		servers := emptyManager.Servers()
		require.NotNil(t, servers)
		assert.Len(t, servers[SGLantern].Outbounds, 0)
		assert.Len(t, servers[SGLantern].Endpoints, 0)
		assert.Len(t, servers[SGUser].Outbounds, 0)
		assert.Len(t, servers[SGUser].Endpoints, 0)
	})
}
