/*
Package multiplex provides a [transport.StreamDialer] that multiplexes connections.
*/
package multiplex

import (
	"context"
	"io"
	"net"
	"sync/atomic"

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
	return &StreamConn{
		Conn:        conn,
		readClosed:  atomic.Bool{},
		writeClosed: atomic.Bool{},
	}, nil
}

// StreamConn is a wrapper around a net.Conn that implements transport.StreamConn.
type StreamConn struct {
	net.Conn

	readClosed  atomic.Bool
	writeClosed atomic.Bool
}

// Read reads data from the connection returning the number of bytes read and any error that occurred.
// If the read side of the connection has been closed, Read returns io.EOF.
func (m *StreamConn) Read(b []byte) (n int, err error) {
	if m.readClosed.Load() {
		return 0, io.EOF
	}
	return m.Conn.Read(b)
}

// Write writes data to the connection returning the number of bytes written and any error that occurred.
// If the write side of the connection has been closed, Write returns io.EOF.
func (m *StreamConn) Write(b []byte) (n int, err error) {
	if m.writeClosed.Load() {
		return 0, io.EOF
	}
	return m.Conn.Write(b)
}

// CloseRead closes the read side of the connection. No more data can be read.
func (m *StreamConn) CloseRead() error {
	m.readClosed.CompareAndSwap(false, true)
	if m.writeClosed.Load() {
		return m.Close()
	}
	return nil
}

// CloseWrite closes the write side of the connection. No more data can be written.
func (m *StreamConn) CloseWrite() error {
	m.writeClosed.CompareAndSwap(false, true)
	if m.readClosed.Load() {
		return m.Close()
	}
	return nil
}
