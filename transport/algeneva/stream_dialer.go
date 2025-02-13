/*
Package algeneva provides a [transport.StreamDialer] that uses the application layer geneva protocol to
route traffic through a proxy server.

HTTP strategies can be found here:
https://github.com/getlantern/algeneva/blob/main/strategies.go

Application layer geneva modifies the request-line or headers of the request using the specified strategy.
Any further modifications to these fields essentially breaks the geneva protocol. Due to this, it
is recommended to only wrap a StreamDialer that does not modify the request-line or headers for it
to be effective, such as the [transport.TCPDialer].
*/
package algeneva

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/golog"

	algeneva "github.com/getlantern/lantern-algeneva"

	"github.com/getlantern/radiance/config"
)

var log = golog.LoggerFor("transport.algeneva")

// StreamDialer routes traffic through an algeneva proxy.
type StreamDialer struct {
	innerSD transport.StreamDialer
	opts    algeneva.DialerOpts
}

// NewStreamDialer creates a new algeneva StreamDialer using the provided configuration.
//
// Note: it is recommended to only wrap a StreamDialer that does not modify the request-line or headers
// such as the [transport.TCPDialer] for the geneva protocol to be effective.
func NewStreamDialer(innerSD transport.StreamDialer, cfg *config.Config) (transport.StreamDialer, error) {
	alcfg := cfg.GetConnectCfgAlgeneva()
	if alcfg == nil {
		return nil, errors.New("no algeneva config found")
	}
	if sd, ok := innerSD.(*transport.TCPDialer); !ok {
		// we need a warn log function
		log.Debugf("Warning: the algeneva protocol will be ineffective if innerSD (%T) modifies the request-line or headers", sd)
	}

	opts := algeneva.DialerOpts{
		AlgenevaStrategy: alcfg.Strategy,
	}
	if len(cfg.CertPem) > 0 {
		certPool := x509.NewCertPool()
		if ok := certPool.AppendCertsFromPEM(cfg.CertPem); !ok {
			return nil, errors.New("failed to append certificate to pool")
		}
		opts.TLSConfig = &tls.Config{
			RootCAs:    certPool,
			ServerName: cfg.Addr,
		}
	}
	return &StreamDialer{
		innerSD: innerSD,
		opts:    opts,
	}, nil
}

// DialStream implements the [transport.StreamDialer] interface.
func (d *StreamDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	sd, err := d.innerSD.DialStream(ctx, remoteAddr)
	if err != nil {
		log.Debugf("innerSD: %v", err)
		return nil, err
	}
	opts := d.opts
	opts.Dialer = &dialer{conn: sd}
	conn, err := algeneva.DialContext(ctx, "tcp", remoteAddr, opts)
	if err != nil {
		log.Debugf("algeneva.DialContext: %v", err)
		return nil, err
	}
	return transport.WrapConn(sd, conn, conn), nil
}

// dialer is a helper struct that implements the [algeneva.Dialer] interface, which requires a Dial
// method. This also allows us to still have access to CloseRead and CloseWrite on the inned StreamConn
// by wrapping it in a dialer and passing it to algeneva in the dialer opts. algeneva will receive
// the established StreamConn when it calls Dial or DialContext.
type dialer struct {
	conn transport.StreamConn
}

func (d *dialer) Dial(network, address string) (net.Conn, error) {
	return d.conn, nil
}

func (d *dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return d.conn, nil
}
