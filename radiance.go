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
	"time"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
)

var (
	log = golog.LoggerFor("radiance")

	configPollInterval = 10 * time.Minute
)

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	srv         *http.Server
	confHandler *config.ConfigHandler
	config      *config.Config
}

// NewRadiance creates a new Radiance server using an existing config.
func NewRadiance() *Radiance {
	return &Radiance{confHandler: config.NewConfigHandler(configPollInterval), config: nil}
}

// Run starts the Radiance proxy server on the specified address.
func (r *Radiance) Run(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conf, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		return err
	}
	r.config = conf

	dialer, err := transport.DialerFrom(conf)
	if err != nil {
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
func (r *Radiance) Shutdown() error {
	return r.srv.Shutdown(context.Background())
}

func (r *Radiance) GetConfig() *config.Config {
	return r.config
}
