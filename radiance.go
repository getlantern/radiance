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

// TUNStatus is a type used for representing the state of the TUN device and routing configuration.
type TUNStatus string

const (
	ConnectedTUNStatus    TUNStatus = "connected"
	DisconnectedTUNStatus TUNStatus = "disconnected"
	ConnectingTUNStatus   TUNStatus = "connecting"
)

// ProxyStatus provide
type ProxyStatus struct {
	Connected bool
	// Location provides the proxy's geographical location. If connected is false,
	// the value will be a empty string.
	Location string
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
// TODO: tunStatus need to be updated when TUN is active
type Radiance struct {
	srv         httpServer
	confHandler configHandler

	connected              bool
	tunStatus              TUNStatus
	statusMutex            sync.Locker
	stopChan               chan struct{}
	proxyStatusListenersMu sync.Locker
	proxyStatusListeners   []chan ProxyStatus
}

// NewRadiance creates a new Radiance server using an existing config.
func NewRadiance() *Radiance {
	return &Radiance{
		confHandler:            config.NewConfigHandler(configPollInterval),
		connected:              false,
		tunStatus:              DisconnectedTUNStatus,
		statusMutex:            new(sync.Mutex),
		proxyStatusListenersMu: new(sync.Mutex),
		stopChan:               make(chan struct{}),
		proxyStatusListeners:   make([]chan ProxyStatus, 0),
	}
}

// Run starts the Radiance proxy server on the specified address.
func (r *Radiance) Run(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conf, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		r.setStatus(false, r.TUNStatus())
		return err
	}

	dialer, err := transport.DialerFrom(conf)
	if err != nil {
		r.setStatus(false, r.TUNStatus())
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

	r.setStatus(true, r.TUNStatus())
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
	if !r.connectionStatus() {
		return nil
	}
	if r.srv == nil {
		return fmt.Errorf("server is nil")
	}
	if err := r.srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown server: %w", err)
	}
	r.confHandler.Stop()
	r.setStatus(false, r.TUNStatus())
	close(r.stopChan)
	return nil
}

func (r *Radiance) connectionStatus() bool {
	r.statusMutex.Lock()
	defer r.statusMutex.Unlock()
	return r.connected
}

func (r *Radiance) setStatus(connected bool, status TUNStatus) {
	r.statusMutex.Lock()
	r.connected = connected
	r.tunStatus = status
	r.statusMutex.Unlock()

	// send notifications in a separate goroutine to avoid blocking the Radiance main loop
	go r.notifyListeners(connected)
}

func (r *Radiance) notifyListeners(connected bool) {
	r.proxyStatusListenersMu.Lock()
	status := ProxyStatus{
		Connected: connected,
		Location:  r.ActiveProxyLocation(context.Background()),
	}
	r.proxyStatusListenersMu.Unlock()
	for _, listener := range r.proxyStatusListeners {
		select {
		case listener <- status:
		default:
		}
	}
}

// TUNStatus checks the current TUN status
func (r *Radiance) TUNStatus() TUNStatus {
	r.statusMutex.Lock()
	defer r.statusMutex.Unlock()
	return r.tunStatus
}

// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
// If the VPN is disconnected, it returns nil.
func (r *Radiance) ActiveProxyLocation(ctx context.Context) string {
	if !r.connectionStatus() {
		log.Debug("VPN is not connected")
		return ""
	}

	config, err := r.confHandler.GetConfig(ctx)
	if err != nil {
		log.Errorf("could not retrieve config: %w", err)
		return ""
	}

	if config == nil {
		log.Errorf("config is nil")
		return ""
	}

	if location := config.GetLocation(); location != nil {
		return location.City
	}
	log.Errorf("could not retrieve location")
	return ""
}

// ProxyStatus provides information about the current proxy status like the proxy's
// location or whether the proxy is connected or not.
func (r *Radiance) ProxyStatus() <-chan ProxyStatus {
	proxyStatus := make(chan ProxyStatus)
	r.proxyStatusListenersMu.Lock()
	r.proxyStatusListeners = append(r.proxyStatusListeners, proxyStatus)
	r.proxyStatusListenersMu.Unlock()
	return proxyStatus
}
