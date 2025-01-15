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

	"github.com/Jigsaw-Code/outline-sdk/network"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
)

var log = golog.LoggerFor("vpn")

// VPN is a VPN client.
type VPN struct {
	device *device
	tun    network.IPDevice
}

// TODO:
// - set system settings for the VPN
// - add tests

// New creates a new VPN client.
func New(conf *config.Config) (*VPN, error) {
	device, err := newDevice(conf)
	if err != nil {
		return nil, fmt.Errorf("failed to create ip device: %w", err)
	}
	return &VPN{device: device}, nil
}

// Start starts the VPN client on localAddr. It blocks until the VPN client is closed.
func (vpn *VPN) Start(localAddr string) error {
	tun, err := openTunIfce(localAddr)
	if err != nil {
		return fmt.Errorf("failed to create tun interface: %w", err)
	}
	vpn.tun = tun

	var t2dErr error
	done := make(chan struct{})
	go func() {
		n, err := io.Copy(vpn.device, vpn.tun)
		log.Debugf("TUN -> Device: %d bytes, %v", n, err)
		t2dErr = err
		close(done)
	}()
	n, err := io.Copy(vpn.tun, vpn.device)
	log.Debugf("Device -> TUN: %d bytes, %v", n, err)
	<-done

	return errors.Join(err, t2dErr)
}

// Stop stops the VPN client and closes the TUN interface.
func (vpn *VPN) Stop() error {
	return vpn.tun.Close()
}
