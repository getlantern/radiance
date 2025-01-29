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
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/vpn/tunnel"
)

var (
	log = golog.LoggerFor("vpn")

	clientMu sync.Mutex
	client   *vpnClient
)

type RoutingConfig struct {
	TunName      string
	TunIP        string
	Gw           string
	Dns          string
	StartRouting bool
}

// vpnClient is a vpn client. It's also a Singleton
type vpnClient struct {
	routeConfig *RoutingConfig
	tunDev      io.ReadWriteCloser
	proxy       *tunnel.Tunnel
	dialer      transport.StreamDialer
	remoteAddr  string
	authToken   string

	running     bool
	isConnected bool
	done        chan struct{}
}

func NewClient(dialer transport.StreamDialer, conf RoutingConfig, remoteAddr, authToken string) (*vpnClient, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if client != nil {
		return client, nil
	}

	log.Debug("initializing VPN client")
	client = &vpnClient{
		routeConfig: &conf,
		dialer:      dialer,
		remoteAddr:  remoteAddr,
		authToken:   authToken,
		done:        make(chan struct{}),
	}
	return client, nil
}

// Start starts the VPN client on localAddr and configures routing.
func (c *vpnClient) Start() (err error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	if c.running {
		return fmt.Errorf("VPN client already running")
	}

	log.Debugf("starting VPN client on local address: %s", c.routeConfig.TunIP)
	c.dialer = newConnectDialer(c.dialer, c.remoteAddr, c.authToken)

	c.tunDev, err = openTunDevice(c.routeConfig)
	if err != nil {
		return fmt.Errorf("failed to open TUN device: %w", err)
	}
	defer func() {
		if err != nil {
			c.Stop()
		}
	}()

	pl := transport.UDPListener{}
	proxy, err := tunnel.NewTunnel(c.dialer, pl, false, c.tunDev)
	if err != nil {
		return fmt.Errorf("failed to create device: %w", err)
	}
	c.proxy = proxy

	go func() {
		log.Debug("Starting to relay from TunDev -> Proxy")
		n, err := io.CopyBuffer(c.proxy, c.tunDev, make([]byte, 1500))
		log.Debugf("TunDev -> Proxy: %d bytes, %v", n, err)
		close(c.done)
	}()
	if c.routeConfig.StartRouting {
		if err := startRouting(c.routeConfig, c.remoteAddr, false); err != nil {
			return err
		}
	}
	log.Debug("client started")
	c.running = true
	c.isConnected = true
	return nil
}

// Stop stops the VPN client and closes the TUN interface.
func (c *vpnClient) Stop() error {
	log.Debug("closing VPN client")
	var allErrs error
	if c.routeConfig.StartRouting {
		if err := stopRouting(c.routeConfig); err != nil {
			log.Error(err)
			allErrs = errors.Join(allErrs, err)
		}
	}
	if err := c.proxy.Close(); err != nil {
		log.Error(err)
		allErrs = errors.Join(allErrs, err)
	}
	if err := c.tunDev.Close(); err != nil {
		log.Error(err)
		allErrs = errors.Join(allErrs, err)
	}

	c.running = false
	c.isConnected = false
	<-c.done
	return allErrs
}

func (c *vpnClient) IsConnected() bool {
	return c.isConnected
}
