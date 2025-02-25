package proxyless

import (
	"context"
	"net"
	"net/http"
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

type ProxylessOutboundOptions struct {
	option.DialerOptions
}

type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	dialer otransport.StreamDialer
	client *http.Client
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options ProxylessOutboundOptions) (adapter.Outbound, error) {
	glog.Debug("creating outbound dialer")
	outboundDialer, err := dialer.New(ctx, options.DialerOptions)
	if err != nil {
		return nil, err
	}

	glog.Debug("getting config")
	// ch := config.NewConfigHandler(time.Minute * 10)
	// configCtx, cancel := context.WithTimeout(ctx, time.Second*30)
	// defer cancel()
	// conf, err := ch.GetConfig(configCtx)
	// if err != nil {
	// 	return nil, err
	// }

	outSD := &sboxSD{
		outSD:  outboundDialer,
		logger: logger,
	}
	dialer, err := proxyless.NewStreamDialer(outSD, &config.ProxyConnectConfig{
		ProtocolConfig: &config.ProxyConnectConfig_ConnectCfgProxyless{
			ConnectCfgProxyless: &config.ProxyConnectConfig_ProxylessConfig{
				ConfigText: "split:2|split:123",
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions("proxyless", tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:  logger,
		dialer:  dialer,
	}, nil
}

func (o *Outbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = o.Tag()
	metadata.Destination = destination
	o.logger.InfoContext(ctx, "received proxyless request to %q domain", metadata.Domain)
	conn, err := o.dialer.DialStream(ctx, metadata.Domain)
	if err != nil {
		o.logger.ErrorContext(ctx, "failed to dial to %q: %w", metadata.Domain, err)
	}
	return conn, err
}

func (o *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
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
