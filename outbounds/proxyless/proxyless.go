// Package proxyless hold a sing-box outbound proxyless implementation that basically
// wraps the proxyless transport and use it for dialing
package proxyless

import (
	"context"
	"fmt"
	"net"
	"os"

	otransport "github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport/proxyless"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
)

var glog = golog.LoggerFor("proxyless")

// ProxylessOutboundOptions set options that will be used when building the outbound dialer.
// A Config must be provided with a config text used by the proxyless dialer.
// A FallbackDialer must be provided so we can use it when proxyless fail
type ProxylessOutboundOptions struct {
	option.DialerOptions
	Config         *config.ProxyConnectConfig
	FallbackDialer otransport.StreamDialer
}

// ProxylessOutbound implements a proxyless outbound
type ProxylessOutbound struct {
	outbound.Adapter
	logger         logger.ContextLogger
	dialer         otransport.StreamDialer
	fallbackDialer otransport.StreamDialer
}

// NewProxylessOutbound creates a proxyless outbond that uses the proxyless transport
// for dialing
func NewProxylessOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options ProxylessOutboundOptions) (adapter.Outbound, error) {
	glog.Debug("creating outbound dialer")
	outboundDialer, err := dialer.New(ctx, options.DialerOptions)
	if err != nil {
		return nil, err
	}

	outSD := &sboxSD{
		outSD:  outboundDialer,
		logger: logger,
	}
	dialer, err := proxyless.NewStreamDialer(outSD, options.Config)
	if err != nil {
		return nil, err
	}

	return &ProxylessOutbound{
		Adapter:        outbound.NewAdapterWithDialerOptions("proxyless", tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:         logger,
		dialer:         dialer,
		fallbackDialer: options.FallbackDialer,
	}, nil
}

// DialContext extracts the metadata domain, add the destination to the context
// and use the proxyless dialer for sending the request
func (o *ProxylessOutbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination

	// trying proxyless
	o.logger.InfoContext(ctx, "received proxyless request to %q (%q) domain", metadata.Domain, destination.String())
	conn, err := o.dialer.DialStream(ctx, fmt.Sprintf("%s:%d", metadata.Domain, destination.Port))
	if err == nil {
		return conn, nil
	}

	// dialing with fallback because proxyless failed
	if o.fallbackDialer == nil {
		return nil, err
	}

	o.logger.ErrorContext(ctx, "failed to dial to proxyless dial %q, using fallback dialer: %w", metadata.Domain, err)
	conn, err = o.fallbackDialer.DialStream(ctx, destination.String())
	if err != nil {
		o.logger.ErrorContext(ctx, "failed to dial to proxyless dial %q with fallback: %w", metadata.Domain, err)
		return nil, err
	}

	return conn, nil
}

// ListenPacket isn't implemented
func (o *ProxylessOutbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

// wrapper around sing-box's network.Dialer to implement streamDialer interface to pass to a
// stream dialer as innerSD
type sboxSD struct {
	outSD  network.Dialer
	logger log.ContextLogger
}

func (s *sboxSD) DialStream(ctx context.Context, addr string) (otransport.StreamConn, error) {
	s.logger.InfoContext(ctx, "proxyless sboxSD dialing ", addr)
	destination := metadata.ParseSocksaddr(addr)
	conn, err := s.outSD.DialContext(ctx, network.NetworkTCP, destination)
	if err != nil {
		s.logger.ErrorContext(ctx, "Error dialing %s: %v", addr, err)
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}
