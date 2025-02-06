/*
Package logger implements a StreamDialer that logs the data written to and read from the connection. This is used for debugging and will be removed in the future.
*/
package logger

import (
	"context"
	"errors"
	"io"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/config"
)

const (
	logBytes = true
	logData  = false
)

var log = golog.LoggerFor("transport.logDialer")

// StreamDialer is a wrapper around a StreamDialer that is used for debugging. Currently, it logs
// the data written to and read from the connection. This will be removed in the future.
type StreamDialer struct {
	innerSD transport.StreamDialer
}

func NewStreamDialer(innerSD transport.StreamDialer, conf *config.Config) (transport.StreamDialer, error) {
	if innerSD == nil {
		return nil, errors.New("dialer must not be nil")
	}

	return &StreamDialer{
		innerSD: innerSD,
	}, nil
}

func (sd *StreamDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	log.Debugf("Dialing %v", addr)
	conn, err := sd.innerSD.DialStream(ctx, addr)
	if err != nil {
		return nil, err
	}

	log.Debugf("Connected to %v", addr)
	rw := &logRW{rw: conn, logBytes: logBytes, logData: logData}
	return transport.WrapConn(conn, rw, rw), nil
}

type logRW struct {
	rw       io.ReadWriter
	logBytes bool
	logData  bool
}

func (c *logRW) Write(p []byte) (n int, err error) {
	n, err = c.rw.Write(p)
	if c.logBytes {
		log.Debugf("Wrote %v/%v bytes", n, len(p))
	}
	if c.logData {
		log.Debug(string(p))
	}
	return n, err
}

func (c *logRW) Read(p []byte) (n int, err error) {
	n, err = c.rw.Read(p)
	if c.logBytes {
		log.Debugf("Read %v bytes", n)
	}
	if c.logData {
		log.Debug(string(p[:n]))
	}
	return n, err
}
