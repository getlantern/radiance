/*
Package radiance provides a local server that proxies all requests to a remote proxy server using different
protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
*/
package radiance

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/client"
	"github.com/getlantern/radiance/common/reporting"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
	"github.com/getlantern/radiance/transport/proxyless"
)

var (
	log          = golog.LoggerFor("radiance")
	vpnLogOutput = "radiance.log"

	configPollInterval = 10 * time.Minute
)

// ErrNotImplemented is returned by functions which have not yet been implemented. The existence of
// this error is temporary; this will go away when the API stabilized.
var ErrNotImplemented = errors.New("not yet implemented")

//go:generate mockgen -destination=radiance_mock_test.go -package=radiance github.com/getlantern/radiance httpServer,configHandler

// httpServer is an interface that abstracts the http.Server struct for easier testing.
type httpServer interface {
	Serve(listener net.Listener) error
	Shutdown(ctx context.Context) error
}

// configHandler is an interface that abstracts the config.ConfigHandler struct for easier testing.
type configHandler interface {
	// GetConfig returns the current proxy configuration.
	GetConfig(ctx context.Context) ([]*config.Config, error)
	// Stop stops the config handler from fetching new configurations.
	Stop()
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	vpnClient client.VPNClient

	srv           httpServer
	confHandler   configHandler
	proxyLocation *atomic.Value

	connected   bool
	statusMutex sync.Locker
	stopChan    chan struct{}
}

// NewRadiance creates a new Radiance server using an existing config.
func NewRadiance() (*Radiance, error) {
	vpnC, err := client.NewVPNClient(vpnLogOutput)
	if err != nil {
		return nil, err
	}
	return &Radiance{
		vpnClient: vpnC,

		confHandler:   config.NewConfigHandler(configPollInterval),
		proxyLocation: new(atomic.Value),
		connected:     false,
		statusMutex:   new(sync.Mutex),
		stopChan:      make(chan struct{}),
	}, nil
}

// Run starts the Radiance proxy server on the specified address.
// This function will be replaced by StartVPN as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) run(addr string) error {
	reporting.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	log.Debug("Fetching config")
	configs, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		r.setStatus(false)
		sentry.CaptureException(err)
		return fmt.Errorf("Could not fetch config: %w", err)
	}

	var proxyConf, proxylessConf *config.Config
	for _, conf := range configs {
		if conf.GetConnectCfgProxyless() != nil {
			proxylessConf = conf
		}
		proxyConf = conf
		r.proxyLocation.Store(proxyConf.GetLocation())
	}

	dialer, err := transport.DialerFrom(proxyConf)
	if err != nil {
		r.setStatus(false)
		sentry.CaptureException(err)
		return fmt.Errorf("Could not create dialer: %w", err)
	}
	log.Debugf("Creating dialer with config: %+v", proxyConf)

	pAddr := fmt.Sprintf("%s:%d", proxyConf.Addr, proxyConf.Port)
	handler := proxyHandler{
		addr:      pAddr,
		authToken: proxyConf.AuthToken,
		dialer:    dialer,
		client: http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.DialStream(ctx, pAddr)
				},
			},
		},
	}

	if proxylessConf != nil {
		handler.proxylessDialer, err = proxyless.NewStreamDialer(dialer, proxylessConf)
		if err != nil {
			sentry.CaptureException(err)
			return fmt.Errorf("could not create proxyless dialer: %w", err)
		}
	}

	r.srv = &http.Server{
		Handler: &handler,
		// Prevent slowloris attacks by setting a read timeout.
		ReadHeaderTimeout: 5 * time.Second,
	}

	r.setStatus(true)
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
// This function will be replaced by StopVPN as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) shutdown(ctx context.Context) error {
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
	r.setStatus(false)
	close(r.stopChan)
	// Flush sentry events before returning
	if result := sentry.Flush(6 * time.Second); !result {
		log.Error("sentry.Flush: timeout")
	}
	return nil
}

// StartVPN starts the local VPN device, configuring routing rules such that network traffic on this
// machine is sent through this instance of Radiance.
//
// This function will be implemented as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) StartVPN() error {
	log.Debug("Starting VPN")
	err := r.vpnClient.Start()
	r.setStatus(err == nil)
	return err
}

// StopVPN stops the local VPN device and removes routing rules configured by StartVPN.
//
// This function will be implemented as part of https://github.com/getlantern/engineering/issues/1883
func (r *Radiance) StopVPN() error {
	log.Debug("Stopping VPN")
	r.setStatus(false)
	return r.vpnClient.Stop()
}

// PauseVPN pauses the VPN connection for the specified duration.
func (r *Radiance) PauseVPN(dur time.Duration) error {
	log.Debugf("Pausing VPN for %v", dur)
	return r.vpnClient.Pause(dur)
}

// ResumeVPN resumes a paused VPN connection.
func (r *Radiance) ResumeVPN() {
	log.Debug("Resuming VPN")
	r.vpnClient.Resume()
}

func (r *Radiance) connectionStatus() bool {
	r.statusMutex.Lock()
	defer r.statusMutex.Unlock()
	return r.connected
}

func (r *Radiance) setStatus(connected bool) {
	r.statusMutex.Lock()
	r.connected = connected
	r.statusMutex.Unlock()

	// send notifications in a separate goroutine to avoid blocking the Radiance main loop
	go func() {
		// Recover from panics to avoid crashing the Radiance main loop
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("Recovered from panic: %v", r)
				reporting.PanicListener(fmt.Sprintf("Recovered from panic: %v", r))
			}
		}()
	}()
}

// ServerLocation is the location of a remote VPN server.
type ServerLocation config.ProxyConnectConfig_ProxyLocation

// Server represents a remote VPN server.
type Server struct {
	Address            string
	Location           ServerLocation
	SupportedProtocols []string
}

// GetServers returns the remote VPN servers currently assigned to this client, as well as the index
// of the active server.
//
// This function will be implemented as part of https://github.com/getlantern/engineering/issues/1920
func (r *Radiance) GetServers() (servers []Server, activeServer int) {
	// TODO: implement me!
	return nil, 0
}

// ActiveProxyLocation returns the proxy server's location if the VPN is connected.
// If the VPN is disconnected, it returns nil.
//
// This function will be removed as part of https://github.com/getlantern/engineering/issues/1920
func (r *Radiance) ActiveProxyLocation(ctx context.Context) string {
	if !r.connectionStatus() {
		log.Debug("VPN is not connected")
		return ""
	}

	if location, ok := r.proxyLocation.Load().(*config.ProxyConnectConfig_ProxyLocation); ok && location != nil {
		return location.City
	}
	log.Errorf("could not retrieve location")
	return ""
}

// IssueReport represents a user report of a bug or service problem. This report can be submitted
// via [Radiance.ReportIssue].
//
// The fields of this type will be defined as part of https://github.com/getlantern/engineering/issues/1921
type IssueReport struct {
}

// ReportIssue submits an issue report to the back-end.
//
// This function will be implemented as part of https://github.com/getlantern/engineering/issues/1921
func (r *Radiance) ReportIssue(report IssueReport) error {
	// TODO: implement me!
	return ErrNotImplemented
}
