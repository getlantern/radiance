package logger

import (
	"context"
	"errors"
	"io"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
)

var log = golog.LoggerFor("LogDialer")

// StreamDialer is a wrapper around a StreamDialer that is used for debugging. Currently, it logs
// the data written to and read from the connection. This will be removed in the future.
type StreamDialer struct {
	innerSD transport.StreamDialer
}

func NewStreamDialer(innerSD transport.StreamDialer, _ config.Config) (transport.StreamDialer, error) {
	if innerSD == nil {
		return nil, errors.New("dialer must not be nil")
	}

	return &StreamDialer{innerSD: innerSD}, nil
}

func (d *StreamDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	stream, err := d.innerSD.DialStream(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}

	log.Debugf("Connected to %v", remoteAddr)
	rw := &logRW{rw: stream}
	return transport.WrapConn(stream, rw, rw), nil
}

type logRW struct {
	rw io.ReadWriter
}

func (c *logRW) Write(p []byte) (n int, err error) {
	log.Debugf("Writing %v bytes", len(p))
	log.Debug(string(p))
	return c.rw.Write(p)
}

func (c *logRW) Read(p []byte) (n int, err error) {
	n, err = c.rw.Read(p)
	log.Debugf("Read %v bytes", n)
	log.Debug(string(p[:n]))
	return
}
