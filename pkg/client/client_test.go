package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/radiance/config"
	"github.com/stretchr/testify/assert"
	gomock "go.uber.org/mock/gomock"
)

func TestNewClient(t *testing.T) {
	var tests = []struct {
		name            string
		givenListenAddr string
		assert          func(*testing.T, *proxyServer, error)
	}{
		{
			name:            "it should return an error with empty listen address",
			givenListenAddr: "",
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.Error(t, err)
				assert.Nil(t, s)
			},
		},
		{
			name:            "it should succeed when providing a valid listen address",
			givenListenAddr: "http://localhost:9999",
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, s)
				assert.NotEmpty(t, s.listenAddr)
				assert.NotEmpty(t, s.status)
				assert.NotNil(t, s.statusMutex)
				assert.NotNil(t, s.radiance)
				assert.NotNil(t, s.stopChan)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewClient(tt.givenListenAddr)
			tt.assert(t, s, err)
		})
	}
}

func TestStartVPN(t *testing.T) {
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *proxyServer
		assert func(*testing.T, *proxyServer, error)
	}{
		{
			name: "it should return an error when failed to start radiance",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      DisconnectedVPNStatus,
				}
				server.EXPECT().Run(gomock.Any()).DoAndReturn(func(_ string) error {
					assert.Equal(t, ConnectingVPNStatus, s.VPNStatus())
					return assert.AnError
				})
				return s
			},
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.Error(t, err)
				assert.Equal(t, DisconnectedVPNStatus, s.VPNStatus())
			},
		},
		{
			name: "it should succeed when starting radiance",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      DisconnectedVPNStatus,
				}
				server.EXPECT().Run(gomock.Any()).DoAndReturn(func(_ string) error {
					assert.Equal(t, ConnectingVPNStatus, s.VPNStatus())
					return nil
				})
				return s
			},
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.NoError(t, err)
				assert.Equal(t, ConnectedVPNStatus, s.VPNStatus())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s := tt.setup(ctrl)
			err := s.StartVPN()
			tt.assert(t, s, err)
		})
	}
}

func TestStopVPN(t *testing.T) {
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *proxyServer
		assert func(*testing.T, *proxyServer, error)
	}{
		{
			name: "it should return nil when VPN is disconnected",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      DisconnectedVPNStatus,
				}
				return s
			},
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.NoError(t, err)
				assert.Equal(t, DisconnectedVPNStatus, s.VPNStatus())
			},
		},
		{
			name: "it should return an error when failed to stop radiance",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      ConnectedVPNStatus,
				}
				server.EXPECT().Shutdown().Return(assert.AnError)
				return s
			},
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.Error(t, err)
				assert.Equal(t, ConnectedVPNStatus, s.VPNStatus())
			},
		},
		{
			name: "it should succeed when stopping radiance",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      ConnectedVPNStatus,
					stopChan:    make(chan struct{}),
				}
				server.EXPECT().Shutdown().Return(nil)
				go func() {
					_, ok := <-s.stopChan
					assert.False(t, ok, "stopChan should be closed")
				}()
				return s
			},
			assert: func(t *testing.T, s *proxyServer, err error) {
				assert.NoError(t, err)
				assert.Equal(t, DisconnectedVPNStatus, s.VPNStatus())
			},
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s := tt.setup(ctrl)
			err := s.StopVPN()
			tt.assert(t, s, err)
		})
	}
}

func TestVPNStatus(t *testing.T) {
	s := &proxyServer{
		statusMutex: new(sync.Mutex),
		status:      ConnectedVPNStatus,
	}
	assert.Equal(t, ConnectedVPNStatus, s.VPNStatus())

	s.setStatus(DisconnectedVPNStatus)
	assert.Equal(t, DisconnectedVPNStatus, s.VPNStatus())

	s.setStatus(ConnectingVPNStatus)
	assert.Equal(t, ConnectingVPNStatus, s.VPNStatus())
}

func TestActiveProxyLocation(t *testing.T) {
	expectedCity := "New York"
	var tests = []struct {
		name   string
		setup  func(*gomock.Controller) *proxyServer
		assert func(*testing.T, *proxyServer, *string, error)
	}{
		{
			name: "it should return nil when VPN is disconnected and return an error",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      DisconnectedVPNStatus,
				}
				return s
			},
			assert: func(t *testing.T, s *proxyServer, location *string, err error) {
				assert.Nil(t, location)
				assert.Error(t, err)
			},
		},
		{
			name: "it should return nil when failed to retrieve config",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      ConnectedVPNStatus,
				}
				server.EXPECT().GetConfig(gomock.Any()).Return(nil, assert.AnError)
				return s
			},
			assert: func(t *testing.T, s *proxyServer, location *string, err error) {
				assert.Nil(t, location)
				assert.Error(t, err)
			},
		},
		{
			name: "it should return the location when VPN is connected",
			setup: func(ctrl *gomock.Controller) *proxyServer {
				server := NewMockserver(ctrl)
				s := &proxyServer{
					radiance:    server,
					statusMutex: new(sync.Mutex),
					status:      ConnectedVPNStatus,
				}
				config := config.Config{
					Location: &config.ProxyConnectConfig_ProxyLocation{City: expectedCity},
				}
				server.EXPECT().GetConfig(gomock.Any()).Return(&config, nil)
				return s
			},
			assert: func(t *testing.T, s *proxyServer, location *string, err error) {
				assert.NoError(t, err)
				assert.NotNil(t, location)
				assert.Equal(t, expectedCity, *location)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			s := tt.setup(ctrl)
			location, err := s.ActiveProxyLocation(context.Background())
			tt.assert(t, s, location, err)
		})
	}
}

func TestProxyStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	expectedCity := "New York"
	server := NewMockserver(ctrl)
	config := config.Config{
		Location: &config.ProxyConnectConfig_ProxyLocation{City: expectedCity},
	}
	server.EXPECT().GetConfig(gomock.Any()).Return(&config, nil)

	s := &proxyServer{
		statusMutex: new(sync.Mutex),
		status:      DisconnectedVPNStatus,
		stopChan:    make(chan struct{}),
		radiance:    server,
	}
	pollInterval := 1 * time.Millisecond
	var statusChan <-chan ProxyStatus
	t.Run("it should return a not connected status", func(t *testing.T) {
		statusChan = s.ProxyStatus(pollInterval)
		assert.NotNil(t, statusChan)
		status, ok := <-statusChan
		assert.True(t, ok)
		assert.False(t, status.Connected)
		assert.Empty(t, status.Location)
	})

	t.Run("it should return the proxy status when VPN is connected", func(t *testing.T) {
		s.setStatus(ConnectedVPNStatus)
		status, ok := <-statusChan
		assert.True(t, ok)
		assert.True(t, status.Connected)
		assert.Equal(t, expectedCity, status.Location)
	})

	t.Run("it should return the proxy status when VPN is disconnected", func(t *testing.T) {
		s.setStatus(DisconnectedVPNStatus)
		status, ok := <-statusChan
		assert.True(t, ok)
		assert.False(t, status.Connected)
		assert.Empty(t, status.Location)
	})

	t.Run("it should close the channel when stopChan is closed", func(t *testing.T) {
		close(s.stopChan)
		_, ok := <-statusChan
		assert.False(t, ok)

		secondStatusChan := s.ProxyStatus(pollInterval)
		_, ok = <-secondStatusChan
		assert.False(t, ok)
	})
}
