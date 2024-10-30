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
	dialer transport.StreamDialer
}

func NewStreamDialer(dialer transport.StreamDialer) (transport.StreamDialer, error) {
	if dialer == nil {
		return nil, errors.New("dialer must not be nil")
	}

	return &logDialer{dialer: dialer}, nil
}

func (d *logDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	stream, err := d.dialer.DialStream(ctx, remoteAddr)
	if err != nil {
		return nil, err
	}

	w := &logWritter{w: stream}
	return transport.WrapConn(stream, stream, w), nil
}

type logWritter struct {
	w io.Writer
}

func (w *logWritter) Write(p []byte) (n int, err error) {
	log.Debugf("Writing %v bytes", len(p))
	return w.w.Write(p)
}
