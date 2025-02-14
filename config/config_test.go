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
	respProxy := testConfigResponse.Proxy.Proxies
	tests := []struct {
		name         string
		configResult configResult
		assert       func(t *testing.T, got []*Config, err error)
	}{
		{
			name:         "returns current config",
			configResult: configResult{cfg: respProxy, err: nil},
			assert: func(t *testing.T, got []*Config, err error) {
				assert.NoError(t, err)
				for i := range got {
					assert.True(t, proto.Equal(respProxy[i], got[i]), "GetConfig should return the current config")
				}
			},
		},
		{
			name:         "error encountered during fetch",
			configResult: configResult{cfg: nil, err: ErrFetchingConfig},
			assert: func(t *testing.T, got []*Config, err error) {
				assert.ErrorIs(t, err, ErrFetchingConfig,
					"GetConfig should return the error encountered during fetch",
				)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // prevent GetConfig from waiting for the context to expire
			ch := &ConfigHandler{config: eventual.NewValue()}
			ch.config.Set(tt.configResult)
			got, err := ch.GetConfig(ctx)
			tt.assert(t, got, err)
		})
	}
}

func TestFetchLoop_UpdateConfig(t *testing.T) {
	buf, _ := proto.Marshal(testConfigResponse)
	confReader := io.NopCloser(bytes.NewReader(buf))
	tests := []struct {
		name     string
		response *http.Response
		err      error
		assert   func(*testing.T, bool, error)
	}{
		{
			name:     "received new config",
			response: &http.Response{StatusCode: http.StatusOK, Body: confReader},
			assert: func(t *testing.T, changed bool, err error) {
				assert.NoError(t, err)
				assert.True(t, changed, "fetchLoop should update the config if a new one is received")
			},
		},
		{
			name:     "no new config available and no error",
			response: &http.Response{StatusCode: http.StatusNoContent},
			assert: func(t *testing.T, changed bool, err error) {
				assert.NoError(t, err)
				assert.False(t, changed, "fetchLoop should not update the config if no new config is received")
			},
		},
		{
			name: "error fetching config",
			err:  assert.AnError,
			assert: func(t *testing.T, changed bool, err error) {
				assert.NoError(t, err)
				assert.False(t, changed, "fetchLoop should not update the config if no new config is received")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			continueC := make(chan struct{}, 1)
			ftr := newFetcher(&http.Client{
				Transport: &mockRoundTripper{
					resp:      tt.response,
					err:       tt.err,
					continueC: continueC,
				},
			})
			ch := &ConfigHandler{
				config: eventual.NewValue(),
				stopC:  make(chan struct{}),
				ftr:    ftr,
			}
			conf := &Config{}
			ch.config.Set(configResult{cfg: []*Config{conf}, err: nil})

			go ch.fetchLoop(0)
			<-continueC
			close(ch.stopC)

			ctx, cancel := context.WithCancel(context.Background())
			cancel() // we don't want GetConfig to wait
			_got, _ := ch.config.Get(ctx)
			got := _got.(configResult)

			tt.assert(t, !proto.Equal(conf, got.cfg[0]), got.err)
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
