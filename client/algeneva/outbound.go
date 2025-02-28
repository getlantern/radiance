package algeneva

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/coder/websocket"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/common/dialer"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
	"github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/common/network"
	sHTTP "github.com/sagernet/sing/protocol/http"

	"github.com/getlantern/algeneva"
	"github.com/getlantern/golog"
)

const authTokenHeader = "X-Lantern-Auth-Token"

var alog = golog.LoggerFor("algeneva")

type AlgenevaOutboundOptions struct {
	option.DialerOptions
	option.ServerOptions
	Strategy string `json:"strategy"`
}

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[AlgenevaOutboundOptions](registry, "algeneva", NewOutbound)
}

type Outbound struct {
	outbound.Adapter
	strategy *algeneva.HTTPStrategy
	client   *sHTTP.Client
	logger   logger.ContextLogger
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options AlgenevaOutboundOptions) (adapter.Outbound, error) {
	alog.Debug("Creating outbound")
	outboundDialer, err := dialer.New(ctx, options.DialerOptions)
	if err != nil {
		return nil, err
	}

	strategy, err := algeneva.NewHTTPStrategy(options.Strategy)
	if err != nil {
		logger.Error("parsing strategy: ", err)
		return nil, fmt.Errorf("parsing strategy: %v", err)
	}

	return &Outbound{
		Adapter: outbound.NewAdapterWithDialerOptions("algeneva", tag, []string{network.NetworkTCP}, options.DialerOptions),
		logger:  logger,
		client: sHTTP.NewClient(sHTTP.Options{
			Dialer: &algenevaDialer{
				Dialer:   outboundDialer,
				logger:   logger,
				strategy: strategy,
			},
			Server: options.ServerOptions.Build(),
		}),
	}, nil
}

func (al *Outbound) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	ctx, metadata := adapter.ExtendContext(ctx)
	metadata.Outbound = al.Tag()
	metadata.Destination = destination
	conn, err := al.client.DialContext(ctx, network, destination)
	if err != nil {
		al.logger.ErrorContext(ctx, "algeneva outbound failed to connect to ", destination, " ", err)
	} else {
		al.logger.InfoContext(ctx, "algeneva outbound connection to ", destination)
	}
	return conn, err
}

func (al *Outbound) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}

type algenevaDialer struct {
	network.Dialer
	strategy *algeneva.HTTPStrategy
	logger   log.ContextLogger
}

func (s *algenevaDialer) DialContext(ctx context.Context, network string, destination metadata.Socksaddr) (net.Conn, error) {
	s.logger.InfoContext(ctx, "algeneva dialing ", destination)
	wsopts := &websocket.DialOptions{
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				DialContext: s.dialContext(destination),
			},
		},
	}
	wsc, _, err := websocket.Dial(ctx, "ws://"+destination.AddrString(), wsopts)
	if err != nil {
		return nil, err
	}

	return websocket.NetConn(ctx, wsc, websocket.MessageBinary), nil
}

func (s *algenevaDialer) dialContext(destination metadata.Socksaddr) func(ctx context.Context, network string, address string) (net.Conn, error) {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := s.Dialer.DialContext(ctx, network, destination)
		if err != nil {
			return nil, err
		}
		err = s.handshake(conn, destination)
		if err != nil {
			return nil, err
		}

		return conn, nil
	}
}

func (s *algenevaDialer) handshake(conn net.Conn, destination metadata.Socksaddr) error {
	s.logger.Debug("performing handshake")
	if destination.Port == 0 {
		destination.Port = 80
	}
	req, err := http.NewRequest("CONNECT", destination.AddrString(), nil)
	if err != nil {
		s.logger.Error("creating handshake request: ", err)
		return err
	}
	uri := destination.String()
	req.RequestURI = uri
	req.Host = uri
	req.URL.Host = uri
	if err = algeneva.WriteRequest(conn, req, s.strategy); err != nil {
		s.logger.Error("writing handshake request: ", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		s.logger.Error("reading handshake response: ", err)
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

func (s *algenevaDialer) ListenPacket(ctx context.Context, destination metadata.Socksaddr) (net.PacketConn, error) {
	return nil, os.ErrInvalid
}
