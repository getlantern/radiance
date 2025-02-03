package proxyless

import (
	context "context"
	"testing"
	"time"

	transport "github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/radiance/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gomock "go.uber.org/mock/gomock"
)

func TestNewStreamDialer(t *testing.T) {
	validSplitConfig := &config.Config{
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgProxylessSplit{
			ConnectCfgProxylessSplit: &config.ProxyConnectConfig_ProxylessConfig{
				ConfigText: "split:2|split:123",
			},
		},
	}
	invalidSplitConfig := &config.Config{
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgProxylessSplit{
			ConnectCfgProxylessSplit: &config.ProxyConnectConfig_ProxylessConfig{
				ConfigText: "split:|split:",
			},
		},
	}

	tlsFragConfig := &config.Config{
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgProxylessTlsfrag{
			ConnectCfgProxylessTlsfrag: &config.ProxyConnectConfig_ProxylessConfig{
				ConfigText: "tlsfrag:10",
			},
		},
	}

	disorderConfig := &config.Config{
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgProxylessDisorder{
			ConnectCfgProxylessDisorder: &config.ProxyConnectConfig_ProxylessConfig{
				ConfigText: "disorder:1",
			},
		},
	}

	var tests = []struct {
		name         string
		givenConfig  *config.Config
		givenInnerSD func(ctrl *gomock.Controller) transport.StreamDialer
		assert       func(t *testing.T, dialer transport.StreamDialer, err error)
	}{
		{
			name:        "it should fail when innerSD is nil",
			givenConfig: validSplitConfig,
			givenInnerSD: func(ctrl *gomock.Controller) transport.StreamDialer {
				return nil
			},
			assert: func(t *testing.T, dialer transport.StreamDialer, err error) {
				assert.Error(t, err)
				assert.Nil(t, dialer)
			},
		},
		{
			name:        "it should fail when config is invalid",
			givenConfig: invalidSplitConfig,
			givenInnerSD: func(ctrl *gomock.Controller) transport.StreamDialer {
				return NewMockStreamDialer(ctrl)
			},
			assert: func(t *testing.T, dialer transport.StreamDialer, err error) {
				assert.Error(t, err)
				assert.Nil(t, dialer)
			},
		},
		{
			name:        "it should succeed with valid config and inner stream dialer",
			givenConfig: validSplitConfig,
			givenInnerSD: func(ctrl *gomock.Controller) transport.StreamDialer {
				return NewMockStreamDialer(ctrl)
			},
			assert: func(t *testing.T, dialer transport.StreamDialer, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, dialer)
				d := dialer.(*StreamDialer)
				assert.NotNil(t, d.innerSD)
				assert.NotNil(t, d.proxylessDialer)
				assert.NotNil(t, d.upstreamStatusCacheMutex)
				assert.NotNil(t, d.upstreamStatusCache)
			},
		},
		{
			name:        "it should succeed with tlsfrag config",
			givenConfig: tlsFragConfig,
			givenInnerSD: func(ctrl *gomock.Controller) transport.StreamDialer {
				return NewMockStreamDialer(ctrl)
			},
			assert: func(t *testing.T, dialer transport.StreamDialer, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, dialer)
				d := dialer.(*StreamDialer)
				assert.NotNil(t, d.innerSD)
				assert.NotNil(t, d.proxylessDialer)
				assert.NotNil(t, d.upstreamStatusCacheMutex)
				assert.NotNil(t, d.upstreamStatusCache)
			},
		},
		{
			name:        "it should succeed with disorder config",
			givenConfig: disorderConfig,
			givenInnerSD: func(ctrl *gomock.Controller) transport.StreamDialer {
				return NewMockStreamDialer(ctrl)
			},
			assert: func(t *testing.T, dialer transport.StreamDialer, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, dialer)
				d := dialer.(*StreamDialer)
				assert.NotNil(t, d.innerSD)
				assert.NotNil(t, d.proxylessDialer)
				assert.NotNil(t, d.upstreamStatusCacheMutex)
				assert.NotNil(t, d.upstreamStatusCache)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			dialer, err := NewStreamDialer(tt.givenInnerSD(ctrl), tt.givenConfig)
			tt.assert(t, dialer, err)
		})
	}
}

func TestDialStream(t *testing.T) {
	validConfig := &config.Config{
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgProxylessSplit{
			ConnectCfgProxylessSplit: &config.ProxyConnectConfig_ProxylessConfig{
				ConfigText: "split:2|split:123",
			},
		},
	}
	remoteAddr := "1.1.1.1"
	var tests = []struct {
		name            string
		dialer          func(ctrl *gomock.Controller) *StreamDialer
		givenContext    context.Context
		givenRemoteAddr string
		assert          func(t *testing.T, conn transport.StreamConn, err error)
	}{
		{
			name: "it should try proxyless dialer when it's the first time",
			dialer: func(ctrl *gomock.Controller) *StreamDialer {
				// innerSD shouldn't be called
				innerSD := NewMockStreamDialer(ctrl)
				dialer, err := NewStreamDialer(innerSD, validConfig)
				require.NoError(t, err)

				d := dialer.(*StreamDialer)
				proxylessDialer := NewMockStreamDialer(ctrl)
				proxylessDialer.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, nil)
				d.proxylessDialer = proxylessDialer
				return d
			},
			givenContext:    context.Background(),
			givenRemoteAddr: remoteAddr,
			assert: func(t *testing.T, conn transport.StreamConn, err error) {
				assert.NoError(t, err)
				assert.Nil(t, conn)
			},
		},
		{
			name: "it should try proxyless dialer when it worked on last try",
			dialer: func(ctrl *gomock.Controller) *StreamDialer {
				innerSD := NewMockStreamDialer(ctrl)
				dialer, err := NewStreamDialer(innerSD, validConfig)
				require.NoError(t, err)

				d := dialer.(*StreamDialer)
				proxylessDialer := NewMockStreamDialer(ctrl)
				proxylessDialer.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, nil)
				d.proxylessDialer = proxylessDialer
				d.updateUpstreamStatus(remoteAddr, validConfig.GetConnectCfgProxylessSplit().GetConfigText(), true)
				return d
			},
			givenContext:    context.Background(),
			givenRemoteAddr: remoteAddr,
			assert: func(t *testing.T, conn transport.StreamConn, err error) {
				assert.NoError(t, err)
				assert.Nil(t, conn)
			},
		},
		{
			name: "it should try proxyless dialer when it have new config",
			dialer: func(ctrl *gomock.Controller) *StreamDialer {
				innerSD := NewMockStreamDialer(ctrl)
				dialer, err := NewStreamDialer(innerSD, validConfig)
				require.NoError(t, err)

				d := dialer.(*StreamDialer)
				proxylessDialer := NewMockStreamDialer(ctrl)
				proxylessDialer.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, nil)
				d.proxylessDialer = proxylessDialer
				d.updateUpstreamStatus(remoteAddr, "split:2", false)
				return d
			},
			givenContext:    context.Background(),
			givenRemoteAddr: remoteAddr,
			assert: func(t *testing.T, conn transport.StreamConn, err error) {
				assert.NoError(t, err)
				assert.Nil(t, conn)
			},
		},
		{
			name: "it should try proxyless dialer when last try was long ago",
			dialer: func(ctrl *gomock.Controller) *StreamDialer {
				innerSD := NewMockStreamDialer(ctrl)
				dialer, err := NewStreamDialer(innerSD, validConfig)
				require.NoError(t, err)

				d := dialer.(*StreamDialer)
				proxylessDialer := NewMockStreamDialer(ctrl)
				proxylessDialer.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, nil)
				d.proxylessDialer = proxylessDialer
				d.upstreamStatusCache[remoteAddr] = upstreamStatus{
					RemoteAddr:    remoteAddr,
					LastSuccess:   time.Now().Add(-48 * time.Hour).Unix(),
					NumberOfTries: 10,
					LastResult:    false,
					ConfigText:    validConfig.GetConnectCfgProxylessSplit().GetConfigText(),
				}
				return d
			},
			givenContext:    context.Background(),
			givenRemoteAddr: remoteAddr,
			assert: func(t *testing.T, conn transport.StreamConn, err error) {
				assert.NoError(t, err)
				assert.Nil(t, conn)
			},
		},
		{
			name: "it should use inner stream dialer when none of the conditions are met",
			dialer: func(ctrl *gomock.Controller) *StreamDialer {
				innerSD := NewMockStreamDialer(ctrl)
				innerSD.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, nil)
				dialer, err := NewStreamDialer(innerSD, validConfig)
				require.NoError(t, err)

				d := dialer.(*StreamDialer)
				d.upstreamStatusCache[remoteAddr] = upstreamStatus{
					RemoteAddr:    remoteAddr,
					LastSuccess:   time.Now().Unix(),
					NumberOfTries: 10,
					LastResult:    false,
					ConfigText:    validConfig.GetConnectCfgProxylessSplit().GetConfigText(),
				}
				return d
			},
			givenContext:    context.Background(),
			givenRemoteAddr: remoteAddr,
			assert: func(t *testing.T, conn transport.StreamConn, err error) {
				assert.NoError(t, err)
				assert.Nil(t, conn)
			},
		},
		{
			name: "it should use inner stream dialer when proxyless dialer fails",
			dialer: func(ctrl *gomock.Controller) *StreamDialer {
				innerSD := NewMockStreamDialer(ctrl)
				innerSD.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, nil)
				dialer, err := NewStreamDialer(innerSD, validConfig)
				require.NoError(t, err)

				d := dialer.(*StreamDialer)
				proxylessDialer := NewMockStreamDialer(ctrl)
				proxylessDialer.EXPECT().DialStream(gomock.Any(), remoteAddr).Return(nil, assert.AnError)
				d.proxylessDialer = proxylessDialer
				return d
			},
			givenContext:    context.Background(),
			givenRemoteAddr: remoteAddr,
			assert: func(t *testing.T, conn transport.StreamConn, err error) {
				assert.NoError(t, err)
				assert.Nil(t, conn)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialer := tt.dialer(gomock.NewController(t))
			conn, err := dialer.DialStream(tt.givenContext, tt.givenRemoteAddr)
			tt.assert(t, conn, err)
		})
	}
}
