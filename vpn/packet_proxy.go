package vpn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/dns"
	"github.com/Jigsaw-Code/outline-sdk/network"
	"github.com/Jigsaw-Code/outline-sdk/network/dnstruncate"
	"github.com/Jigsaw-Code/outline-sdk/transport"
	"golang.org/x/net/dns/dnsmessage"
)

// based on https://github.com/Jigsaw-Code/outline-sdk/blob/v0.0.17/x/examples/outline-cli/outline_packet_proxy.go
type packetProxy struct {
	network.DelegatePacketProxy

	remote, fallback network.PacketProxy
	remotePl         transport.PacketListener
}

func newPacketProxy(addr string) *packetProxy {
	pp := &packetProxy{
		remotePl: transport.UDPListener{Address: addr},
	}

	// don't need to check error here, as it will always be nil
	pp.remote, _ = network.NewPacketProxyFromPacketListener(pp.remotePl)
	pp.fallback, _ = dnstruncate.NewPacketProxy()
	pp.DelegatePacketProxy, _ = network.NewDelegatePacketProxy(pp.fallback)

	return pp
}

func (proxy *packetProxy) testConnectivity(resolverAddr, domain string) error {
	dialer := transport.PacketListenerDialer{Listener: proxy.remotePl}
	dnsResolver := dns.NewUDPResolver(dialer, resolverAddr)

	// condensed version of https://github.com/Jigsaw-Code/outline-sdk/blob/v0.0.17/x/connectivity/connectivity.go#L72
	q, err := dns.NewQuestion(domain, dnsmessage.TypeA)
	if err != nil {
		return fmt.Errorf("failed to create DNS question: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = dnsResolver.Query(ctx, *q)
	switch {
	case errors.Is(err, dns.ErrBadRequest):
		return fmt.Errorf("connectivity test failed: %w", err)
	case errors.Is(err, dns.ErrDial) || errors.Is(err, dns.ErrSend) || errors.Is(err, dns.ErrReceive):
		log.Debug("remote server cannot handle UDP traffic, switch to DNS truncate mode.")
		return proxy.SetProxy(proxy.fallback)
	}

	log.Debug("remote server supports UDP, we will delegate all UDP packets to it")
	return proxy.SetProxy(proxy.remote)
}
