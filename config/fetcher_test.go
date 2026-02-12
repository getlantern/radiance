package config

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	C "github.com/getlantern/common"
	"github.com/getlantern/dnstt"
	"github.com/getlantern/kindling"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/common/settings"
	rkindling "github.com/getlantern/radiance/kindling"
	"github.com/getlantern/radiance/kindling/fronted"
)

func TestDomainFrontingFetchConfig(t *testing.T) {
	// Disable this test for now since it depends on external service.
	t.Skip("Skipping TestDomainFrontingFetchConfig since it depends on external service.")
	dataDir := t.TempDir()
	f, err := fronted.NewFronted(context.Background(), reporting.PanicListener, filepath.Join(dataDir, "fronted_cache.json"), io.Discard)
	require.NoError(t, err)
	k := kindling.NewKindling(
		"radiance-df-test",
		kindling.WithDomainFronting(f),
	)
	rkindling.SetKindling(k)
	fetcher := newFetcher("en-US", &api.APIClient{})

	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	_, err = fetcher.fetchConfig(context.Background(), C.ServerLocation{Country: "US"}, privateKey.PublicKey().String())
	// We expect a 500 error since the user does not have any matching tracks.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no lantern-cloud tracks")
}

func TestProxylessFetchConfig(t *testing.T) {
	// Disable this test for now since it depends on external service.
	t.Skip("Skipping TestProxylessFetchConfig since it depends on external service.")
	k := kindling.NewKindling(
		"radiance-df-test",
		kindling.WithProxyless("df.iantem.io"),
	)
	rkindling.SetKindling(k)
	fetcher := newFetcher("en-US", &api.APIClient{})

	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	_, err = fetcher.fetchConfig(context.Background(), C.ServerLocation{Country: "US"}, privateKey.PublicKey().String())
	// We expect a 500 error since the user does not have any matching tracks.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no lantern-cloud tracks")

}

func TestAMPFetchConfig(t *testing.T) {
	// Disable this test for now since it depends on external service.
	t.Skip("Skipping TestAMPFetchConfig since it depends on external service.")
	ampPublicKey := ""
	ampClient, err := fronted.NewAMPClient(context.Background(), os.Stderr, ampPublicKey)
	require.NoError(t, err)
	k := kindling.NewKindling(
		"radiance-df-test",
		kindling.WithAMPCache(ampClient),
	)
	httpClient := k.NewHTTPClient()
	mockUser := &mockUser{}
	fetcher := newFetcher(httpClient, mockUser, "en-US", &api.APIClient{})

	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	_, err = fetcher.fetchConfig(context.Background(), C.ServerLocation{Country: "US"}, privateKey.PublicKey().String())
	// We expect a 500 error since the user does not have any matching tracks.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no lantern-cloud tracks")
}

func TestDNSTTFetchConfig(t *testing.T) {
	t.Skip("Skipping TestDNSTTFetchConfig since it depends on external service.")
	cli, err := dnstt.NewDNSTT(
		dnstt.WithDoH("https://cloudflare-dns.com/dns-query"),
		dnstt.WithTunnelDomain("t.iantem.io"),
		dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
	)
	require.NoError(t, err)
	dnsTunnel2, err := dnstt.NewDNSTT(
		dnstt.WithTunnelDomain("t.iantem.io"),
		dnstt.WithDoH("https://dns.adguard-dns.com/dns-query"),
		dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
	)
	require.NoError(t, err)
	dnsTunnel3, err := dnstt.NewDNSTT(
		dnstt.WithTunnelDomain("t.iantem.io"),
		dnstt.WithDoH("https://dns.google/dns-query"),
		dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
	)
	require.NoError(t, err)
	dnsTunnel4, err := dnstt.NewDNSTT(
		dnstt.WithTunnelDomain("t.iantem.io"),
		dnstt.WithDoT("dns.quad9.net:853"),
		dnstt.WithPublicKey("405eb9e22d806e3a0a8e667c6665a321c8a6a35fa680ed814716a66d7ad84977"),
	)
	require.NoError(t, err)

	k := kindling.NewKindling(
		"radiance-df-test",
		kindling.WithDNSTunnel(cli),
		kindling.WithDNSTunnel(dnsTunnel2),
		kindling.WithDNSTunnel(dnsTunnel3),
		kindling.WithDNSTunnel(dnsTunnel4),
	)
	httpClient := k.NewHTTPClient()
	mockUser := &mockUser{}
	fetcher := newFetcher(httpClient, mockUser, "en-US", &api.APIClient{})

	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	_, err = fetcher.fetchConfig(context.Background(), C.ServerLocation{Country: "US"}, privateKey.PublicKey().String())
	// We expect a 500 error since the user does not have any matching tracks.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no lantern-cloud tracks")
}

type mockRoundTripper struct {
	req  *http.Request
	resp *http.Response
	err  error
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	m.req = req
	return m.resp, m.err
}

func TestFetchConfig(t *testing.T) {
	settings.InitSettings(t.TempDir())
	settings.Set(settings.DeviceIDKey, "mock-device-id")
	settings.Set(settings.UserIDKey, 1234567890)
	settings.Set(settings.TokenKey, "mock-legacy-token")

	privateKey, err := wgtypes.GenerateKey()
	require.NoError(t, err)

	tests := []struct {
		name                 string
		preferredServerLoc   *C.ServerLocation
		mockResponse         *http.Response
		mockError            error
		expectedConfig       []byte
		expectedErrorMessage string
	}{
		{
			name: "successful fetch with new config",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader(func() []byte {
					data := []byte(`{"key":"value"}`)
					return data
				}())),
			},
			expectedConfig: []byte(`{"key":"value"}`),
		},
		{
			name: "no new config available",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockResponse: &http.Response{
				StatusCode: http.StatusNotModified,
				Body:       io.NopCloser(bytes.NewReader(nil)),
			},
			expectedConfig: nil,
		},
		{
			name: "error during request",
			preferredServerLoc: &C.ServerLocation{
				Country: "US",
			},
			mockError:            context.DeadlineExceeded,
			expectedErrorMessage: "context deadline exceeded",
		},
	}

	apiClient := &api.APIClient{}
	defer apiClient.Reset()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRT := &mockRoundTripper{
				resp: tt.mockResponse,
				err:  tt.mockError,
			}
			rkindling.SetKindling(&mockKindling{
				&http.Client{
					Transport: mockRT,
				},
			})
			fetcher := newFetcher("en-US", &api.APIClient{})

			gotConfig, err := fetcher.fetchConfig(t.Context(), *tt.preferredServerLoc, privateKey.PublicKey().String())

			if tt.expectedErrorMessage != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectedErrorMessage)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedConfig, gotConfig)
			}

			if tt.mockResponse != nil {
				require.NotNil(t, mockRT.req)
				assert.Equal(t, "application/json", mockRT.req.Header.Get("Content-Type"))
				assert.Equal(t, "no-cache", mockRT.req.Header.Get("Cache-Control"))

				body, err := io.ReadAll(mockRT.req.Body)
				require.NoError(t, err)

				var confReq C.ConfigRequest
				err = json.Unmarshal(body, &confReq)
				require.NoError(t, err)

				assert.Equal(t, common.Platform, confReq.Platform)
				assert.Equal(t, common.Name, confReq.AppName)
				assert.Equal(t, settings.GetString(settings.DeviceIDKey), confReq.DeviceID)
				assert.Equal(t, privateKey.PublicKey().String(), confReq.WGPublicKey)
				if tt.preferredServerLoc != nil {
					assert.Equal(t, tt.preferredServerLoc, confReq.PreferredLocation)
				}
			}
		})
	}
}

type mockKindling struct {
	c *http.Client
}

// NewHTTPClient returns a new HTTP client that is configured to use kindling.
func (m *mockKindling) NewHTTPClient() *http.Client {
	return m.c
}

// ReplaceTransport replaces an existing transport RoundTripper generator with the provided one.
func (m *mockKindling) ReplaceTransport(name string, rt func(ctx context.Context, addr string) (http.RoundTripper, error)) error {
	panic("not implemented") // TODO: Implement
}
