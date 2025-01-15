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

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/vpn"
)

const (
	VPN_Router   RouterType = "vpn"
	Proxy_Router RouterType = "proxy"
)

var (
	log = golog.LoggerFor("radiance")

	configPollInterval = 10 * time.Minute
)

// RouterType represents the type of router to use for routing traffic.
type RouterType string

// router routes traffic over a local address to its destination. This can be directly or indirectly.
type router interface {
	Start(localAddr string) error
	Stop() error
}

// Radiance is a local server that proxies all requests to a remote proxy server over a transport.StreamDialer.
type Radiance struct {
	confHandler *config.ConfigHandler
	router      router
	routerType  RouterType
}

// New creates a new Radiance instance that will route traffic over a VPN or a proxy server, routerType.
func New(typ RouterType) *Radiance {
	return &Radiance{
		confHandler: config.NewConfigHandler(configPollInterval),
		routerType:  typ,
	}
}

// Run runs the Radiance instance on localAddr.
func (r *Radiance) Run(localAddr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conf, err := r.confHandler.GetConfig(ctx)
	cancel()
	if err != nil {
		return err
	}

	switch r.routerType {
	case VPN_Router:
		r.router, err = vpn.New(conf)
	case Proxy_Router:
		r.router, err = newProxy(conf)
	default:
		return fmt.Errorf("unknown router type: %v", r.routerType)
	}
	if err != nil {
		return err
	}

	return r.router.Start(localAddr)
}
