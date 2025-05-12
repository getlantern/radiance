package boxservice

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/getlantern/sing-box-extensions/protocol"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/client/boxoptions"
)

func TestSelectCustomServer(t *testing.T) {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	options := boxoptions.Options("stderr")
	options.Outbounds = append(options.Outbounds, option.Outbound{Type: "direct", Tag: "testing-out"})

	dataDir := t.TempDir()
	logFactory := log.NewNOPFactory()
	manager := NewCustomServerManager(ctx, dataDir)
	require.NotNil(t, manager)

	// add router to context
	router, err := route.NewRouter(ctx, logFactory, option.RouteOptions{}, common.PtrValueOrDefault(options.DNS))
	require.NoError(t, err)
	service.ContextWithPtr(ctx, router)
	service.ContextWithPtr(ctx, &logFactory)

	// add outbound manager to context
	endpointManager := endpoint.NewManager(logFactory.NewLogger("endpoint"), endpointRegistry)
	// outboundManager := outbound.NewManager(logFactory.NewLogger("outbound"), outboundRegistry, endpointManager, "")
	outboundManager := &mockOutboundManager{}
	require.NoError(t, outboundManager.Create(ctx, router, logFactory.NewLogger("direct"), "direct", constant.TypeDirect, &option.DirectOutboundOptions{}))
	require.NoError(t, outboundManager.Create(ctx, router, logFactory.NewLogger("selector"), CustomSelectorTag, constant.TypeSelector, &option.SelectorOutboundOptions{
		Outbounds:                 []string{"direct"},
		Default:                   "direct",
		InterruptExistConnections: true,
	}))
	service.MustRegister[adapter.EndpointManager](ctx, endpointManager)
	service.MustRegister[adapter.OutboundManager](ctx, outboundManager)
	service.MustRegister[log.Factory](ctx, logFactory)
	manager.ctx = ctx

	// If we're adding an endpoint with wireguard, a wireguard inbound is required
	customConfig := `{
		"tag": "custom-algeneva",
		"outbound": {
			"type": "algeneva",
			"tag": "custom-algeneva",
			"server": "103.104.245.192",
			"server_port": 80,
			"headers": {
				"x-auth-token": "token"
			},
			"tls": {
				"enabled": true,
				"disable_sni": false,
				"server_name": "",
				"insecure": false,
				"min_version": "",
				"max_version": "",
				"cipher_suites": [],
				"certificate": ""
			},
			"strategy": "[HTTP:method:*]-insert{%0A:end:value:4}-|"
		}
	}`
	outboundTag := "custom-algeneva"

	t.Run("it should successfully add algeneva outbound", func(t *testing.T) {
		err = manager.AddCustomServer([]byte(customConfig))
		assert.NoError(t, err)

		// checking if algeneva-out was included as an outbound and route
		outboundManager := service.FromContext[adapter.OutboundManager](manager.ctx)
		_, exists := outboundManager.Outbound(outboundTag)
		assert.True(t, exists)
	})

	t.Run("listing custom servers should return the stored list", func(t *testing.T) {
		customServers, err := manager.ListCustomServers()
		assert.NoError(t, err)
		assert.Len(t, customServers, 1)
		assert.Equal(t, outboundTag, customServers[0].Tag)
	})

	t.Run("selecting custom server should set the default outbound", func(t *testing.T) {
		err = manager.SelectCustomServer(outboundTag)
		require.NoError(t, err)

		outboundManager := service.FromContext[adapter.OutboundManager](manager.ctx)
		outbound, ok := outboundManager.Outbound(CustomSelectorTag)
		assert.True(t, ok)
		selector, ok := outbound.(selector)
		assert.True(t, ok)
		assert.Equal(t, outboundTag, selector.Now())
	})

	t.Run("it should remove the outbound tag", func(t *testing.T) {
		err = manager.RemoveCustomServer(outboundTag)
		assert.NoError(t, err)
		outboundManager := service.FromContext[adapter.OutboundManager](manager.ctx)
		_, exists := outboundManager.Outbound(outboundTag)
		assert.False(t, exists)
	})

	t.Run("listing custom servers should return a empty list because we've removed on the last test", func(t *testing.T) {
		customServers, err := manager.ListCustomServers()
		assert.NoError(t, err)
		assert.Empty(t, customServers)
	})
}

func TestServerManagerIntegration(t *testing.T) {
	inboundRegistry, outboundRegistry, endpointRegistry := protocol.GetRegistries()
	ctx := box.Context(
		context.Background(),
		inboundRegistry,
		outboundRegistry,
		endpointRegistry,
	)

	options := boxoptions.Options("stderr")

	dataDir := t.TempDir()
	logFactory := log.NewNOPFactory()
	manager := NewCustomServerManager(ctx, dataDir)
	require.NotNil(t, manager)

	// add router to context
	router, err := route.NewRouter(ctx, logFactory, option.RouteOptions{}, common.PtrValueOrDefault(options.DNS))
	require.NoError(t, err)
	service.ContextWithPtr(ctx, router)
	service.ContextWithPtr(ctx, &logFactory)

	// add outbound manager to context
	endpointManager := endpoint.NewManager(logFactory.NewLogger("endpoint"), endpointRegistry)
	outboundManager := &mockOutboundManager{}

	require.NoError(t, outboundManager.Create(ctx, router, logFactory.NewLogger("direct"), "direct", constant.TypeDirect, &option.DirectOutboundOptions{}))
	require.NoError(t, outboundManager.Create(ctx, router, logFactory.NewLogger("selector"), CustomSelectorTag, constant.TypeSelector, &option.SelectorOutboundOptions{
		Outbounds:                 []string{"direct"},
		Default:                   "direct",
		InterruptExistConnections: true,
	}))
	service.MustRegister[adapter.EndpointManager](ctx, endpointManager)
	service.MustRegister[adapter.OutboundManager](ctx, outboundManager)
	service.MustRegister[log.Factory](ctx, logFactory)
	manager.ctx = ctx

	srv := newLanternServerManagerMock()
	defer srv.Close()
	parsedURL, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(parsedURL.Port())

	trustingCallback := func(ip string, details []CertDetail) *CertDetail {
		return &details[0]
	}
	t.Run("convert a token into a custom server", func(t *testing.T) {
		require.NoError(t, manager.AddServerManagerInstance("s1", parsedURL.Hostname(), port, "rootToken", trustingCallback))
		customServers, err := manager.ListCustomServers()
		assert.NoError(t, err)
		assert.Len(t, customServers, 1)
		assert.Equal(t, "s1", customServers[0].Tag)
	})

	t.Run("invite a user", func(t *testing.T) {
		inviteToken, err := manager.InviteToServerManagerInstance(parsedURL.Hostname(), port, "rootToken", "invite1")
		assert.NoError(t, err)
		assert.NotEmpty(t, inviteToken)

		require.NoError(t, manager.AddServerManagerInstance("s2", parsedURL.Hostname(), port, inviteToken, trustingCallback))
		customServers, err := manager.ListCustomServers()
		assert.NoError(t, err)
		assert.Len(t, customServers, 2)
		assert.Equal(t, "s2", customServers[1].Tag)

		t.Run("revoke user access", func(t *testing.T) {
			require.NoError(t, manager.RevokeServerManagerInvite(parsedURL.Hostname(), port, "rootToken", "invite1"))
			// trying to access again with the same token should fail
			require.Error(t, manager.AddServerManagerInstance("s1", parsedURL.Hostname(), port, inviteToken, trustingCallback))
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
