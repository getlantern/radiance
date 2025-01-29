/*
Package radiance provides a local server that proxies all requests to a remote proxy server using different
protocols meant to circumvent censorship. Radiance uses a [transport.StreamDialer] to dial the target server
over the desired protocol. The [config.Config] is used to configure the dialer for a proxy server.
*/
package radiance

import (
	"context"
	"fmt"
	"time"

	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/proxy"
	"github.com/getlantern/radiance/transport"
)

var (
	log = golog.LoggerFor("radiance")

	configPollInterval = 10 * time.Minute
)

type client interface {
	Start() error
	Stop() error
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	client      client
	confHandler *config.ConfigHandler
}

// New creates a new Radiance server using an existing config.
func New() *Radiance {
	return &Radiance{confHandler: config.NewConfigHandler(configPollInterval)}
}

// Run starts the Radiance proxy server on the specified address.
func (r *Radiance) Run(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conf, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		return err
	}

	log.Debugf("Creating dialer with config: %+v", conf)
	dialer, err := transport.DialerFrom(conf)
	if err != nil {
		return fmt.Errorf("Could not create dialer: %w", err)
	}

	r.client = proxy.New(dialer, conf.Addr, conf.AuthToken, addr)
	return r.client.Start()
}

func waitForConfig(ctx context.Context, ch *config.ConfigHandler) (*config.Config, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			log.Debug("waiting for config")
		case <-time.After(400 * time.Millisecond):
			proxies, _ := ch.GetConfig(eventual.DontWait)
			if proxies != nil {
				return proxies, nil
			}
		}
	}
}
