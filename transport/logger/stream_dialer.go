package logger

import (
	"context"
	"errors"
	"io"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"
)

var log = golog.LoggerFor("LogDialer")

type logDialer struct {
	innerSD transport.StreamDialer
}

func NewStreamDialer(innerSD transport.StreamDialer) (transport.StreamDialer, error) {
	if innerSD == nil {
		return nil, errors.New("dialer must not be nil")
	}

	return &logDialer{innerSD: innerSD}, nil
}

func (d *logDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
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
	return c.rw.Write(p)
}

func (c *logRW) Read(p []byte) (n int, err error) {
	n, err = c.rw.Read(p)
	log.Debugf("Read %v bytes", n)
	return
}
