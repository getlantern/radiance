package client

import (
	"context"
	"fmt"
	"sync"

	"github.com/getlantern/radiance"
	"github.com/getlantern/radiance/config"
)

type proxyServer struct {
	listenAddr  string
	status      VPNStatus
	statusMutex sync.Locker
	radiance    server
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
	config, err := s.radiance.GetConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not retrieve config: %w", err)
	}

	if s.VPNStatus() == DisconnectedVPNStatus || config == nil {
		return nil, fmt.Errorf("VPN is not connected")
	}

	if location := config.GetLocation(); location != nil {
		return &location.City, nil
	}
	return nil, fmt.Errorf("could not retrieve location")
}

// BandwidthStatus retrieve the current bandwidth usage for use by data cap.
// It returns a JSON string containing the data used and data cap in bytes.
func (s *proxyServer) BandwidthStatus() string {
	panic("not implemented") // TODO: Implement
}

// SetSystemProxy configures the system proxy to route traffic through a specific proxy.
func (s *proxyServer) SetSystemProxy(serverAddr string, port int) error {
	panic("not implemented") // TODO: Implement
}

// ClearSystemProxy reset the system proxy settings to their default (no proxy).
func (s *proxyServer) ClearSystemProxy() error {
	panic("not implemented") // TODO: Implement
}
