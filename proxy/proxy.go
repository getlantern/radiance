package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/getlantern/golog"

	otransport "github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/transport"
)

var log = golog.LoggerFor("proxy")

// Proxy is a local server that pipes all request to a proxy over a transport.StreamDialer,
// attaching the auth token to each request.
type Proxy struct {
	*http.Server
	conf   config.Config
	dialer otransport.StreamDialer
}

// NewProxy creates a new Proxy. config is used to create the transport.StreamDialer that will be
// used to dial the target server.
func NewProxy(config config.Config) (*Proxy, error) {
	log.Debugf("Creating proxy with config: %+v", config)

	dialer, err := transport.DialerFrom(config)
	if err != nil {
		return nil, fmt.Errorf("Could not create dialer: %v", err)
	}

	p := &Proxy{
		conf:   config,
		dialer: dialer,
	}
	server := &http.Server{
		Handler: http.HandlerFunc(p.requestHandler),
	}
	p.Server = server
	return p, nil
}

// ListenAndServe starts the proxy server on the given address.
func (p *Proxy) ListenAndServe(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("Could not listen on %v: %w", addr, err)
	}

	log.Debugf("Listening on %v", addr)
	return p.Serve(listener)
}

// requestHandler attaches the auth token to the request and sends it to the target server.
// The connection is then piped between the client and the target server until it's closed.
func (p *Proxy) requestHandler(resp http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodConnect {
		http.Error(resp, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	targetConn, err := p.dialer.DialStream(req.Context(), p.conf.Addr)
	if err != nil {
		log.Errorf("Failed to dial target: %v", err)
		http.Error(resp, "Failed to dial target", http.StatusServiceUnavailable)
		return
	}
	defer targetConn.Close()

	hijacker, ok := resp.(http.Hijacker)
	if !ok {
		http.Error(resp, "request doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, clientRW, err := hijacker.Hijack()
	if err != nil {
		log.Errorf("Failed to hijack connection: %v", err)
		http.Error(resp, "Failed to hijack connection", http.StatusInternalServerError)
		return
	}
	// We're responsible for closing the client connection since we hijacked it. We close it while
	// respecting the original request context.
	context.AfterFunc(req.Context(), func() { clientConn.Close() })

	if req.URL.Scheme == "" {
		req.URL.Scheme = "http"
	}
	req.Header.Add("X-Lantern-Auth-Token", p.conf.AuthToken)
	if err := req.Write(targetConn); err != nil {
		log.Errorf("Failed to write target request: %v", err)
		return
	}

	go func() {
		io.Copy(targetConn, clientRW)
	}()
	clientRW.ReadFrom(targetConn)
	clientRW.Flush()
}
