package radiance

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
)

var (
	// Placeholders to use in the request headers. These will be replaced with real values when the
	// ability to fetch the config is implemented.
	clientVersion = "9999.9999"
	version       = "9999.9999"
	userId        = "23409"
	proToken      = ""

	log = golog.LoggerFor("radiance")
)

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	srv    *http.Server
	config config.Config
}

// NewRadiance creates a new Radiance server using an existing config.
func NewRadiance() (*Radiance, error) {
	conf, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	log.Debugf("Creating radiance with config: %+v", conf)

	dialer, err := transport.DialerFrom(conf)
	if err != nil {
		return nil, fmt.Errorf("Could not create dialer: %w", err)
	}

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

	return &Radiance{
		srv:    &http.Server{Handler: &handler},
		config: conf,
	}, nil
}

// Run starts the Radiance proxy server on the specified address.
func (r *Radiance) Run(addr string) error {
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
