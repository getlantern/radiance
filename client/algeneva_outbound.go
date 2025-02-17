package client

import (
	"context"
	"net"
	"net/http"
	"os"
	"time"

	otransport "github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	sHTTP "github.com/sagernet/sing/protocol/http"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
)

const authTokenHeader = "X-Lantern-Auth-Token"

var alog = golog.LoggerFor("algeneva")

type AlgenevaOutboundOptions struct {
	option.DialerOptions
	Strategy string
}

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[AlgenevaOutboundOptions](registry, "algeneva", NewOutbound)
}

type Outbound struct {
	outbound.Adapter
	logger logger.ContextLogger
	dialer otransport.StreamDialer
	client *sHTTP.Client
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options AlgenevaOutboundOptions) (adapter.Outbound, error) {
	alog.Debug("Creating outbound")
	outboundDialer, err := dialer.New(ctx, options.DialerOptions)
	if err != nil {
		return nil, err
	}
	alog.Debug("getting config")
	ch := config.NewConfigHandler(time.Minute * 10)
	conf, err := ch.GetConfig(ctx)
	if err != nil {
		return nil, err
	}

	outSD := &sboxSD{
		outSD:  outboundDialer,
		logger: logger,
	}
	dialer, err := transport.DialerFromWithBase(outSD, conf[0])
	if err != nil {
		return nil, err
	}

	srvOpts := option.ServerOptions{
		Server:     conf[0].Addr,
		ServerPort: uint16(conf[0].Port),
	}
	header := http.Header{}
	header.Set(authTokenHeader, conf[0].AuthToken)

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions("algeneva", tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:  logger,
		dialer:  dialer,
		client: sHTTP.NewClient(sHTTP.Options{
			Dialer:  &sdBoxDialer{sd: outSD, logger: logger},
			Server:  srvOpts.Build(),
			Headers: header,
		}),
	}, nil
}

func (al *Outbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = al.Tag()
	metadata.Destination = destination
	conn, err := al.client.DialContext(ctx, network, destination)
	if err != nil {
		al.logger.InfoContext(ctx, "algeneva outbound connection to ", destination)
	} else {
		al.logger.ErrorContext(ctx, "algeneva outbound failed to connect to ", destination, err)
	}
	return conn, err
}

func (al *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

// wrapper around sing-box's network.Dialer to implement streamDialer interface to pass to a
// stream dialer as innerSD
type sboxSD struct {
	outSD  network.Dialer
	logger log.ContextLogger
}

func (s *sboxSD) DialStream(ctx context.Context, addr string) (otransport.StreamConn, error) {
	s.logger.InfoContext(ctx, "algeneva sboxSD dialing ", addr)
	destination := metadata.ParseSocksaddr(addr)
	conn, err := s.outSD.DialContext(ctx, network.NetworkTCP, destination)
	if err != nil {
		s.logger.ErrorContext(ctx, "Error dialing %s: %v", addr, err)
		return nil, err
	}
	return conn.(*net.TCPConn), nil
}

// wrapper around stream dialer to implement sing-box's network.Dialer interface
type sdBoxDialer struct {
	network.Dialer
	sd     otransport.StreamDialer
	logger log.ContextLogger
}

func (s *sdBoxDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	s.logger.InfoContext(ctx, "algeneva sdBoxDialer dialing ", destination)
	return s.sd.DialStream(ctx, destination.String())
}

func (s *sdBoxDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}
