package client

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/getlantern/radiance"
	"github.com/getlantern/radiance/config"
)

type proxyServer struct {
	listenAddr  string
	status      VPNStatus
	statusMutex sync.Locker
	radiance    server
	stopChan    chan struct{}
}

//go:generate mockgen -destination ./client_mock_test.go -source client.go -package client server

type server interface {
	Run(addr string) error
	Shutdown() error
	GetConfig(ctx context.Context) (*config.Config, error)
}

// NewClient creates a new proxy server instance.
func NewClient(laddr string) (*proxyServer, error) {
	if laddr == "" {
		return nil, fmt.Errorf("missing listen address parameter")
	}

	return &proxyServer{
		listenAddr:  laddr,
		radiance:    radiance.NewRadiance(),
		status:      DisconnectedVPNStatus,
		statusMutex: new(sync.Mutex),
		stopChan:    make(chan struct{}),
	}, nil
}

func (s *proxyServer) setStatus(status VPNStatus) {
	s.statusMutex.Lock()
	s.status = status
	s.statusMutex.Unlock()
}

// StartVPN selects a proxy internally and start the VPN.
func (s *proxyServer) StartVPN() error {
	s.setStatus(ConnectingVPNStatus)
	if err := s.radiance.Run(s.listenAddr); err != nil {
		s.setStatus(DisconnectedVPNStatus)
		return fmt.Errorf("failed to start radiance: %w", err)
	}
	s.setStatus(ConnectedVPNStatus)
	return nil
}

// StopVPN stops the VPN and closes the TUN device.
func (s *proxyServer) StopVPN() error {
	if s.VPNStatus() == DisconnectedVPNStatus {
		return nil
	}

	if err := s.radiance.Shutdown(); err != nil {
		return fmt.Errorf("failed to stop radiance: %w", err)
	}
	close(s.stopChan)
	s.setStatus(DisconnectedVPNStatus)
	return nil
}

// VPNStatus checks the current VPN status
func (s *proxyServer) VPNStatus() VPNStatus {
	s.statusMutex.Lock()
	defer s.statusMutex.Unlock()
	return s.status
}

// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
// If the VPN is disconnected, it returns nil.
func (s *proxyServer) ActiveProxyLocation(ctx context.Context) (*string, error) {
	if s.VPNStatus() == DisconnectedVPNStatus {
		return nil, fmt.Errorf("VPN is not connected")
	}

	config, err := s.radiance.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve config: %w", err)
	}

	if config == nil {
		return nil, fmt.Errorf("config is nil")
	}

	if location := config.GetLocation(); location != nil {
		return &location.City, nil
	}
	return nil, fmt.Errorf("could not retrieve location")
}

// ProxyStatus provides information about the current proxy status like the proxy's
// location or whether the proxy is connected or not.
func (s *proxyServer) ProxyStatus(pollInterval time.Duration) <-chan ProxyStatus {
	proxyStatus := make(chan ProxyStatus)
	go func() {
		for {
			select {
			case <-s.stopChan:
				close(proxyStatus)
				return
			case <-time.After(pollInterval):
				if s.VPNStatus() != ConnectedVPNStatus {
					proxyStatus <- ProxyStatus{Connected: false}
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
				location, err := s.ActiveProxyLocation(ctx)
				cancel()
				if err != nil {
					proxyStatus <- ProxyStatus{Connected: false}
					continue
				}
				proxyStatus <- ProxyStatus{
					Connected: true,
					Location:  *location,
				}
			}
		}
	}()
	return proxyStatus
}

// SetSystemProxy configures the system proxy to route traffic through a specific proxy.
func (s *proxyServer) SetSystemProxy(serverAddr string, port int) error {
	panic("not implemented") // TODO: Implement
}

// ClearSystemProxy reset the system proxy settings to their default (no proxy).
func (s *proxyServer) ClearSystemProxy() error {
	panic("not implemented") // TODO: Implement
}
