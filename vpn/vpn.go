/*
vpn package provides a VPN client that sends and receives packets over a TUN interface using a
[transport.StreamDialer].

Currently, only TUN interfaces using IPv4 are supported. With the exception of DNS packets, all other
UDP packets are dropped as they are not supported by the current implementation. UDP DNS packets are
will be sent over TCP.

Windows users must have an OpenVPN client installed.
*/
package vpn

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
)

var (
	log = golog.LoggerFor("vpn")

	clientMu sync.Mutex
	client   *vpnClient
)

// vpnClient is a vpn client. It's also a Singleton
type vpnClient struct {
	tunnel        *tunnel
	tunConfig     TunConfig
	configHandler *config.ConfigHandler

	started atomic.Bool
}

func NewVPNClient(conf TunConfig) (*vpnClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}
	log.Debug("initializing VPN client")
	client = &vpnClient{
		configHandler: config.NewConfigHandler(2 * time.Minute),
		tunConfig:     conf,
		started:       atomic.Bool{},
	}
	return client, nil
}

// Start starts the VPN client on localAddr and configures routing.
func (c *vpnClient) Start() (err error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c.started.Load() {
		return fmt.Errorf("VPN client already started")
	}

	log.Debugf("starting VPN client with local address: %s", c.tunConfig.Addr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	proxyConf, err := c.configHandler.GetConfig(ctx)
	if err != nil {
		return errors.New("failed to get config")
	}

	tun, err := newTunnel(c.tunConfig, proxyConf)
	if err != nil {
		return fmt.Errorf("failed to create tunnel: %w", err)
	}

	c.tunnel = tun
	go tun.start()
	log.Debug("client started")
	c.started.Store(true)
	return nil
}

// Stop stops the VPN client and closes the TUN interface.
func (c *vpnClient) Stop() error {
	log.Debug("stopping client")
	return c.tunnel.close()
}

func (c *vpnClient) IsConnected() bool {
	return c.tunnel.connected.Load()
}
