package multiplex

import (
	"context"
	"net"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/cmux/v2"
	"github.com/getlantern/golog"
	"github.com/xtaci/smux"

	"github.com/getlantern/radiance/config"
)

var log = golog.LoggerFor("transport.multiplexDialer")

// StreamDialer is a wrapper that multiplexes connections.
type StreamDialer struct {
	dialer cmux.DialFN
}

// NewStreamDialer creates a new multiplexing StreamDialer wrapping innerSD.
func NewStreamDialer(innerSD transport.StreamDialer, _ *config.Config) (transport.StreamDialer, error) {
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
	return &StreamDialer{dialer}, nil
}

// DialStream implements the transport.StreamDialer interface.
func (m *StreamDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	conn, err := m.dialer(ctx, "", addr)
	if err != nil {
		log.Errorf("Error dialing %v: %v", addr, err)
		return nil, err
	}
	log.Debugf("dialed %v", addr)
	return &StreamConn{conn}, nil
}

// StreamConn is a wrapper around a net.Conn that implements transport.StreamConn.
type StreamConn struct {
	net.Conn
}

// CloseRead does nothing.
func (m *StreamConn) CloseRead() error { return nil }

// CloseWrite does nothing.
func (m *StreamConn) CloseWrite() error { return nil }
