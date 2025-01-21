/*
Package radiance provides a local server that proxies all requests to a remote proxy server using different
protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
*/
package radiance

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
)

var (
	log = golog.LoggerFor("radiance")

	configPollInterval = 10 * time.Minute
)

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance httpServer,configHandler

// httpServer is an interface that abstracts the http.Server struct for easier testing.
type httpServer interface {
	Serve(listener net.Listener) error
	Shutdown(ctx context.Context) error
}

// configHandler is an interface that abstracts the config.ConfigHandler struct for easier testing.
type configHandler interface {
	// GetConfig returns the current proxy configuration.
	GetConfig(ctx context.Context) (*config.Config, error)
	// Stop stops the config handler from fetching new configurations.
	Stop()
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	srv         httpServer
	confHandler configHandler

	status      VPNStatus
	statusMutex sync.Locker
	stopChan    chan struct{}
}

// NewRadiance creates a new Radiance server using an existing config.
func NewRadiance() *Radiance {
	return &Radiance{
		confHandler: config.NewConfigHandler(configPollInterval),
		status:      DisconnectedVPNStatus,
		statusMutex: new(sync.Mutex),
		stopChan:    make(chan struct{}),
	}
}

// Run starts the Radiance proxy server on the specified address.
func (r *Radiance) Run(addr string) error {
	r.setStatus(ConnectingVPNStatus)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conf, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		r.setStatus(DisconnectedVPNStatus)
		return err
	}

	dialer, err := transport.DialerFrom(conf)
	if err != nil {
		r.setStatus(DisconnectedVPNStatus)
		return fmt.Errorf("Could not create dialer: %w", err)
	}
	log.Debugf("Creating dialer with config: %+v", conf)

	handler := proxyHandler{
		addr:      conf.Addr,
		authToken: conf.AuthToken,
		dialer:    dialer,
		client: http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.DialStream(ctx, conf.Addr)
				},
			},
		},
	}
	r.srv = &http.Server{Handler: &handler}

	r.setStatus(ConnectedVPNStatus)
	return r.listenAndServe(addr)
}

func (r *Radiance) listenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("Could not listen on %v: %w", addr, err)
	}

	log.Debugf("Listening on %v", addr)
	return r.srv.Serve(listener)
}

// Shutdown stops the Radiance server.
func (r *Radiance) Shutdown(ctx context.Context) error {
	if r.VPNStatus() == DisconnectedVPNStatus {
		return nil
	}
	if r.srv == nil {
		return fmt.Errorf("server is nil")
	}
	if err := r.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown server: %w", err)
	}
	r.confHandler.Stop()
	r.setStatus(DisconnectedVPNStatus)
	close(r.stopChan)
	return nil
}

func (s *Radiance) setStatus(status VPNStatus) {
	s.statusMutex.Lock()
	s.status = status
	s.statusMutex.Unlock()
}

// VPNStatus checks the current VPN status
func (r *Radiance) VPNStatus() VPNStatus {
	r.statusMutex.Lock()
	defer r.statusMutex.Unlock()
	return r.status
}

// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
// If the VPN is disconnected, it returns nil.
func (r *Radiance) ActiveProxyLocation(ctx context.Context) (*string, error) {
	if r.VPNStatus() == DisconnectedVPNStatus {
		return nil, fmt.Errorf("VPN is not connected")
	}

	config, err := r.confHandler.GetConfig(ctx)
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
func (r *Radiance) ProxyStatus(pollInterval time.Duration) <-chan ProxyStatus {
	proxyStatus := make(chan ProxyStatus)
	go func() {
		for {
			select {
			case <-r.stopChan:
				close(proxyStatus)
				return
			case <-time.After(pollInterval):
				if r.VPNStatus() != ConnectedVPNStatus {
					proxyStatus <- ProxyStatus{Connected: false}
					continue
				}

				ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
				location, err := r.ActiveProxyLocation(ctx)
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
