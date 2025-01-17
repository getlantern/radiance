package vpn

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"

	"github.com/Jigsaw-Code/outline-sdk/network"
	"github.com/Jigsaw-Code/outline-sdk/network/lwip2transport"
	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/eycorsican/go-tun2socks/tun"

	"github.com/getlantern/radiance/config"
	rtransport "github.com/getlantern/radiance/transport"
)

type TunConfig struct {
	Name string
	Addr string
	Gw   string
	Mask string
	Dns  []string
}

// tunnel is a IO device that sends and receives packets to a remote server.
type tunnel struct {
	proxyDev  network.IPDevice
	tunDev    io.ReadWriteCloser
	sd        transport.StreamDialer
	pp        network.PacketProxy
	proxyAddr net.IP

	connected atomic.Bool
}

// newTunnel creates a new tunnel using the provided configuration to create a [transport.StreamDialer].
func newTunnel(tunConf TunConfig, proxyConf *config.Config) (*tunnel, error) {
	log.Debugf("creating tunnel to %s", proxyConf.Addr)
	sd, err := rtransport.DialerFrom(proxyConf)
	if err != nil {
		return nil, fmt.Errorf("failed to create dialer: %w", err)
	}
	pp := newPacketProxy(proxyConf.Addr)
	lwipDevice, err := lwip2transport.ConfigureDevice(sd, pp)
	if err != nil {
		return nil, fmt.Errorf("failed to configure LWIP device: %w", err)
	}

	tunDev, err := tun.OpenTunDevice(
		tunConf.Name, tunConf.Addr, tunConf.Gw,
		tunConf.Mask, tunConf.Dns, false,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to open TUN device: %w", err)
	}

	addr := net.ParseIP(proxyConf.Addr)
	return &tunnel{
		proxyDev:  lwipDevice,
		tunDev:    tunDev,
		sd:        sd,
		pp:        pp,
		proxyAddr: addr,
		connected: atomic.Bool{},
	}, nil
}

func (t *tunnel) close() error {
	log.Debug("closing tunnel")
	return errors.Join(t.tunDev.Close(), t.proxyDev.Close())
}

func (t *tunnel) start() {
	t.connected.Store(true)
	go func() {
		n, err := io.Copy(t.proxyDev, t.tunDev)
		log.Debugf("TunDev -> ProxyDev: %d bytes, %v", n, err)
	}()
	n, err := io.Copy(t.tunDev, t.proxyDev)
	log.Debugf("ProxyDev -> TunDev: %d bytes, %v", n, err)
	t.connected.Store(false)
}
