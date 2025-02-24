package proxyless_outbound

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	otransport "github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport/proxyless"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
)

var log = golog.LoggerFor("proxyless")

type ProxylessOutboundOptions struct {
	option.DialerOptions
}

type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	dialer otransport.StreamDialer
	client *http.Client
}

func NewOutbound(ctx context.Context, router adapter.Router, logger logger.ContextLogger, tag string, options ProxylessOutboundOptions) (adapter.Outbound, error) {
	log.Debug("creating outbound dialer")
	_, err := dialer.New(ctx, options.DialerOptions)
	if err != nil {
		return nil, err
	}

	log.Debug("getting config")
	ch := config.NewConfigHandler(time.Minute * 10)
	cctx, cancel := context.WithTimeout(ctx, time.Second*30)
	defer cancel()
	conf, err := ch.GetConfig(cctx)
	if err != nil {
		return nil, err
	}

	dialer, err := proxyless.NewStreamDialer(nil, conf[0])
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
	conn, err := o.dialer.DialStream(ctx, metadata.Domain)
	if err != nil {
		o.logger.ErrorContext(ctx, "failed to dial to %q: %w", metadata.Domain, err)
	}
	return conn, err
}

func (o *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}
