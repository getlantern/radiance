package config

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/getlantern/eventual/v2"
	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/proto"
)

func TestConfigHandler_GetConfig(t *testing.T) {
	respProxy := testConfigResponse.Proxy.Proxies[0]
	tests := []struct {
		name          string
		configHandler func() *ConfigHandler
		assert        func(t *testing.T, got *Config, err error)
	}{
		{
			name: "returns current config",
			configHandler: func() *ConfigHandler {
				ch := &ConfigHandler{config: eventual.NewValue()}
				ch.config.Set(respProxy)
				return ch
			},
			assert: func(t *testing.T, got *Config, err error) {
				assert.NoError(t, err)
				assert.True(t, proto.Equal(respProxy, got), "GetConfig should return the current config")
			},
		},
		{
			name: "timeout while fetching",
			configHandler: func() *ConfigHandler {
				ch := &ConfigHandler{config: eventual.NewValue()}
				ch.isFetching.Store(true)
				return ch
			},
			assert: func(t *testing.T, got *Config, err error) {
				assert.Nilf(t, got, "GetConfig should return nil if context expires")
				assert.ErrorIsf(t, err, ErrFetchingConfig,
					"GetConfig should return ErrFetchingConfig if fetch is in progress",
				)
			},
		},
		{
			name: "returns error on failed fetch",
			configHandler: func() *ConfigHandler {
				return &ConfigHandler{config: eventual.NewValue(), err: assert.AnError}
			},
			assert: func(t *testing.T, got *Config, err error) {
				assert.Nilf(t, got, "GetConfig should return nil if fetch failed")
				assert.Equalf(t, assert.AnError, err,
					"GetConfig should return the error encountered during fetch",
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // prevent GetConfig from waiting for the context to expire
			got, err := tt.configHandler().GetConfig(ctx)
			tt.assert(t, got, err)
		})
	}
}

func TestFetchConfig_UpdateConfig(t *testing.T) {
	buf, _ := proto.Marshal(testConfigResponse)
	confReader := io.NopCloser(bytes.NewReader(buf))
	tests := []struct {
		name     string
		response *http.Response
		assert   func(*testing.T, bool)
	}{
		{
			name:     "received new config",
			response: &http.Response{StatusCode: http.StatusOK, Body: confReader},
			assert: func(t *testing.T, changed bool) {
				assert.True(t, changed, "FetchConfig should update the config if a new one is received")
			},
		},
		{
			name:     "did not receive new config",
			response: &http.Response{StatusCode: http.StatusNoContent},
			assert: func(t *testing.T, changed bool) {
				assert.False(t, changed, "FetchConfig should not update the config if no new config is received")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done := make(chan struct{}, 1)
			ch := &ConfigHandler{
				config: eventual.NewValue(),
				fetcher: newFetcher(&http.Client{
					Transport: &mockRoundTripper{
						resp: tt.response,
						done: done,
					},
				}),
			}
			conf := &Config{}
			ch.config.Set(conf)
			ch.FetchConfig()
			<-done

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // we don't want GetConfig to wait
			got, _ := ch.config.Get(ctx)

			tt.assert(t, !proto.Equal(conf, got.(*Config)))
		})
	}
}

var testConfigResponse = &ConfigResponse{
	Country: "US",
	Proxy: &ConfigResponse_Proxy{
		Proxies: []*ProxyConnectConfig{{
			Track:    "track",
			Protocol: "protocol",
		}},
	},
}
