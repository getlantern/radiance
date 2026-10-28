//go:build !novpn

package ipc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/vpn"
	"github.com/stretchr/testify/require"
)

func TestSplitTunnelStateAPI(t *testing.T) {
	// Backend initialization changes process-wide logging and crash output.
	if os.Getenv("RADIANCE_SPLIT_API_CHILD") == "" {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSplitTunnelStateAPI$")
		cmd.Env = append(os.Environ(), "RADIANCE_SPLIT_API_CHILD=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}
	settings.Reset()
	t.Cleanup(settings.Reset)
	dataDir, logDir := t.TempDir(), t.TempDir()
	t.Setenv("RADIANCE_DATA_PATH", dataDir)
	t.Setenv("RADIANCE_LOG_PATH", logDir)
	require.NoError(t, settings.InitSettings(dataDir))
	require.NoError(t, settings.Patch(settings.Settings{
		settings.DataPathKey: dataDir,
		settings.LogPathKey:  logDir,
	}))
	b, err := backend.NewLocalBackend(t.Context(), backend.Options{
		DataDir: dataDir, LogDir: logDir, LogLevel: "error",
	})
	require.NoError(t, err)
	t.Cleanup(b.Close)
	c := splitTunnelTestClient(newLocalAPI(b, false))
	ctx := t.Context()

	state, err := c.SplitTunnelState(ctx)
	require.NoError(t, err)
	require.False(t, state.Enabled)
	require.Equal(t, vpn.SplitTunnelPolicyExclude, state.Policy)

	selected := vpn.SplitTunnelFilter{
		DomainSuffix:     []string{".ir"},
		ProcessName:      []string{"browser"},
		ProcessPath:      []string{"/missing/browser"},
		ProcessPathRegex: []string{"/Applications/Browser[.]app/.*"},
		PackageName:      []string{"org.example.browser"},
	}
	require.NoError(t, c.AddSplitTunnelItems(ctx, selected))
	require.NoError(t, c.EnableSplitTunneling(ctx, true))
	state, err = c.SwitchSplitTunnelPolicy(ctx, vpn.SplitTunnelPolicyInclude)
	require.NoError(t, err)
	require.True(t, state.Enabled)
	require.Equal(t, vpn.SplitTunnelPolicyInclude, state.Policy)
	require.Empty(t, state.Filters.DomainSuffix)
	require.Empty(t, state.Filters.ProcessName)
	require.Empty(t, state.Filters.ProcessPath)
	require.Empty(t, state.Filters.ProcessPathRegex)
	require.Empty(t, state.Filters.PackageName)

	require.NoError(t, c.AddSplitTunnelItems(ctx, selected))
	state, err = c.SwitchSplitTunnelPolicy(ctx, vpn.SplitTunnelPolicyInclude)
	require.NoError(t, err)
	require.Equal(t, selected, state.Filters)
	filters, err := c.SplitTunnelFilters(ctx)
	require.NoError(t, err)
	require.Equal(t, selected, filters)

	state, err = c.ClearSplitTunnelApps(ctx)
	require.NoError(t, err)
	require.Equal(t, selected.DomainSuffix, state.Filters.DomainSuffix)
	require.Empty(t, state.Filters.ProcessName)
	require.Empty(t, state.Filters.ProcessPath)
	require.Empty(t, state.Filters.ProcessPathRegex)
	require.Empty(t, state.Filters.PackageName)
	require.Equal(t, vpn.SplitTunnelPolicyInclude, state.Policy)

	require.NoError(t, settings.Set(settings.SplitTunnelPolicyKey, "exclude"))
	prefs, err := c.Settings(ctx)
	require.NoError(t, err)
	require.Equal(t, "include", prefs[settings.SplitTunnelPolicyKey])
	require.Equal(t, true, prefs[settings.SplitTunnelKey])

	rulePath := filepath.Join(dataDir, internal.SplitTunnelFileName)
	require.NoError(t, os.Rename(rulePath, rulePath+".saved"))
	require.NoError(t, os.Mkdir(rulePath, 0o700))
	_, err = c.SwitchSplitTunnelPolicy(ctx, vpn.SplitTunnelPolicyExclude)
	var ipcErr *Error
	require.ErrorAs(t, err, &ipcErr)
	require.Equal(t, http.StatusInternalServerError, ipcErr.Status)
	afterFailure, err := c.SplitTunnelState(ctx)
	require.NoError(t, err)
	require.Equal(t, state, afterFailure)
}

func TestSplitTunnelRoutesRejectInvalidRequests(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	for _, body := range []string{"{", `{}`, `null`, `{"policy":"bogus"}`, `{"policy":true}`} {
		t.Run(body, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, splitTunnelPolicyEndpoint, strings.NewReader(body))
			response := httptest.NewRecorder()
			api.ServeHTTP(response, request)
			require.Equal(t, http.StatusBadRequest, response.Code)
		})
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, splitTunnelStateEndpoint},
		{http.MethodGet, splitTunnelPolicyEndpoint},
		{http.MethodPost, splitTunnelAppsEndpoint},
		{http.MethodPut, splitTunnelEndpoint},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			response := httptest.NewRecorder()
			api.ServeHTTP(response, request)
			require.Equal(t, http.StatusMethodNotAllowed, response.Code)
		})
	}
}

func TestSplitTunnelRoutesRequireAuthorizedPeer(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, true)
	api.ownerSID.Store(new("owner"))
	for _, route := range []struct{ method, path string }{
		{http.MethodGet, splitTunnelStateEndpoint},
		{http.MethodPut, splitTunnelPolicyEndpoint},
		{http.MethodDelete, splitTunnelAppsEndpoint},
	} {
		response := servePeerRequest(api, usr{uid: "other"}, route.method, route.path, `{"policy":"include"}`)
		require.Equal(t, http.StatusForbidden, response.Code)
		response = servePeerRequest(api, usr{}, route.method, route.path, `{"policy":"include"}`)
		require.Equal(t, http.StatusUnauthorized, response.Code)
	}
	for _, peer := range []usr{{uid: "owner"}, {uid: "admin", isAdmin: true}} {
		response := servePeerRequest(api, peer, http.MethodPut, splitTunnelPolicyEndpoint, `{"policy":"bogus"}`)
		require.Equal(t, http.StatusBadRequest, response.Code)
	}
}

func TestSplitTunnelClientErrors(t *testing.T) {
	calls := 0
	c := splitTunnelTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	_, err := c.SwitchSplitTunnelPolicy(t.Context(), "bogus")
	require.Error(t, err)
	require.Zero(t, calls)
	for _, operation := range []func(context.Context) (vpn.SplitTunnelState, error){
		c.SplitTunnelState,
		func(ctx context.Context) (vpn.SplitTunnelState, error) {
			return c.SwitchSplitTunnelPolicy(ctx, vpn.SplitTunnelPolicyInclude)
		},
		c.ClearSplitTunnelApps,
	} {
		_, err := operation(t.Context())
		require.True(t, IsNotFound(err))
	}

	c = splitTunnelTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		require.Equal(t, splitTunnelPolicyEndpoint, r.URL.Path)
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, map[string]string{"policy": "include"}, body)
		http.Error(w, "saved split-tunnel policy could not be applied", http.StatusInternalServerError)
	}))
	_, err = c.SwitchSplitTunnelPolicy(t.Context(), vpn.SplitTunnelPolicyInclude)
	var ipcErr *Error
	require.ErrorAs(t, err, &ipcErr)
	require.Equal(t, http.StatusInternalServerError, ipcErr.Status)
	require.Contains(t, ipcErr.Message, "could not be applied")
}

type splitTunnelTransport struct{ handler http.Handler }

func (t splitTunnelTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.handler.ServeHTTP(w, r)
	return w.Result(), nil
}

func splitTunnelTestClient(handler http.Handler) *Client {
	return &Client{http: &http.Client{Transport: splitTunnelTransport{handler: handler}}}
}
