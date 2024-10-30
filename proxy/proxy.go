package proxy

import (
	"fmt"
	"net"
	"net/http"

	"github.com/Jigsaw-Code/outline-sdk/x/httpproxy"
	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/transport"
)

var log = golog.LoggerFor("proxy")

type Proxy struct {
	http.Server
}

func NewProxy(proxyConfig string) (*Proxy, error) {
	log.Debugf("Creating proxy with config: %v", proxyConfig)

	dialer, err := transport.DialerFrom(proxyConfig)
	if err != nil {
		return nil, fmt.Errorf("Could not create dialer: %v", err)
	}

	proxyHandler := httpproxy.NewProxyHandler(dialer)
	server := &Proxy{http.Server{Handler: proxyHandler}}

	return server, nil
}

func (p *Proxy) ListenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("Could not listen on %v: %w", addr, err)
	}

	log.Debugf("Listening on %v", addr)
	return p.Serve(listener)
}
