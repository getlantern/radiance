package multiplex

import (
	"context"
	"net"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/cmux/v2"
	"github.com/getlantern/golog"
	"github.com/xtaci/smux"
)

var log = golog.LoggerFor("MultiplexDialer")

type StreamDialer struct {
	innerSD transport.StreamDialer
	dialer  cmux.DialFN
}

func NewStreamDialer(innerSD transport.StreamDialer) transport.StreamDialer {
	dialer := cmux.Dialer(
		&cmux.DialerOpts{
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				log.Debugf("innerSD dialing %v", addr)
				return innerSD.DialStream(ctx, addr)
			},
			PoolSize: 1,
			Protocol: cmux.NewSmuxProtocol(smux.DefaultConfig()),
		},
	)
	return &StreamDialer{innerSD, dialer}
}

func (m *StreamDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	conn, err := m.dialer(ctx, "", addr)
	log.Debugf("dialed %v", addr)
	if err != nil {
		log.Errorf("Error dialing %v: %v", addr, err)
		return nil, err
	}
	return &StreamConn{conn}, nil
}

type StreamConn struct {
	net.Conn
}

func (m *StreamConn) CloseRead() error {
	return m.Close()
}

func (m *StreamConn) CloseWrite() error {
	return m.Close()
}
