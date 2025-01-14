// Package consumption implements a transport that register the data usage
// consumed in bytes and maintain the data cap.
package consumption

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/config"
)

var (
	log = golog.LoggerFor("transport.consumptionDialer")
	// DataSent store the total number of bytes sent.
	DataSent = new(atomic.Uint64)
	// DataRecv store the total number of bytes received.
	DataRecv = new(atomic.Uint64)
)

//go:generate mockgen -destination=./stream_dialer_mock_test.go -package=consumption github.com/Jigsaw-Code/outline-sdk/transport StreamDialer,StreamConn

// StreamDialer is a wrapper around a StreamDialer that is used for registering data usage.
type StreamDialer struct {
	innerSD transport.StreamDialer
}

// NewStreamDialer creates a new StreamDialer that registers data usage.
func NewStreamDialer(innerSD transport.StreamDialer, _ *config.Config) (transport.StreamDialer, error) {
	if innerSD == nil {
		return nil, errors.New("dialer must not be nil")
	}
	return &StreamDialer{innerSD: innerSD}, nil
}

// DialStream implements the transport.StreamDialer interface.
func (c *StreamDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	stream, err := c.innerSD.DialStream(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}

	rw := &rw{stream: stream}
	return transport.WrapConn(stream, rw, rw), nil
}

type rw struct {
	stream transport.StreamConn
}

func (c *rw) Write(p []byte) (n int, err error) {
	n, err = c.stream.Write(p)
	if n > 0 {
		DataSent.Add(uint64(n))
	}
	return n, err
}

func (c *rw) Read(p []byte) (n int, err error) {
	n, err = c.stream.Read(p)
	if n > 0 {
		DataRecv.Add(uint64(n))
	}
	return n, err
}
