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

const (
	appVersionHeader = "X-Lantern-App-Version"
	versionHeader    = "X-Lantern-Version"
	platformHeader   = "X-Lantern-Platform"
	appNameHeader    = "X-Lantern-App"
	deviceIdHeader   = "X-Lantern-Device-Id"
	userIdHeader     = "X-Lantern-User-Id"
	authTokenHeader  = "X-Lantern-Auth-Token"
)

var (
	// Placeholders to use in the request headers. These will be replaced with real values when the
	// ability to fetch the config is implemented.
	clientVersion = "9999.9999"
	version       = "9999.9999"
	userId        = "23409"
	proToken      = ""
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
		return nil, fmt.Errorf("Could not create dialer: %w", err)
	}
	// dialer, _ = logger.NewStreamDialer(dialer, config)

	handler := proxyHandler{
		addr:      config.Addr,
		authToken: config.AuthToken,
		dialer:    dialer,
		client: http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.DialStream(ctx, config.Addr)
				},
			},
		},
	}

	return &Proxy{
		conf:   config,
		dialer: dialer,
		Server: &http.Server{
			Handler: &handler,
		},
	}, nil
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

// proxyHandler sends all requests over dialer to the proxy server. Requests are handled differently
// based on whether they are CONNECT requests or not. CONNECT requests, which are usually https, will
// establish a tunnel to the target server. All other requests are forwarded to the proxy server to
// let it decide how to handle them.
type proxyHandler struct {
	// addr is the address of the proxy server.
	addr string
	// authToken is the authentication token to send with each request to the proxy server.
	authToken string
	dialer    otransport.StreamDialer
	// client is an http client that will be used to forward non-CONNECT requests to the proxy server.
	client http.Client
}

func (h *proxyHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		h.handleConnect(resp, req)
		return
	}
	h.handleNonConnect(resp, req)
}

// handleConnect handles CONNECT requests by dialing the proxy server and sending the CONNECT request
// with the required headers. After the connection is established, data is piped between the client
// and the target.
func (h *proxyHandler) handleConnect(proxyResp http.ResponseWriter, proxyReq *http.Request) {
	targetConn, err := h.dialer.DialStream(proxyReq.Context(), h.addr)
	if err != nil {
		sendError(proxyResp, "Failed to dial target", http.StatusServiceUnavailable, err)
		return
	}
	defer targetConn.Close()

	hijacker, ok := proxyResp.(http.Hijacker)
	if !ok {
		http.Error(proxyResp, "request doesn't support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		sendError(proxyResp, "Failed to hijack connection", http.StatusInternalServerError, err)
		return
	}

	log.Debug("hijacked connection, writing connect request")

	// We're responsible for closing the client connection since we hijacked it. But since we're
	// just piping data back and forth, we can give that responsibility back to the server, which
	// will close the connection when the request is done.
	context.AfterFunc(proxyReq.Context(), func() { clientConn.Close() })

	// Create a new CONNECT request to send to the proxy server.
	connectReq, err := http.NewRequestWithContext(
		proxyReq.Context(),
		http.MethodConnect,
		proxyReq.URL.String(),
		nil,
	)
	addRequiredHeaders(connectReq, h.authToken)
	if err = connectReq.Write(targetConn); err != nil {
		sendError(proxyResp, "Failed to write connect request to proxy", http.StatusInternalServerError, err)
		return
	}

	// Pipe data between the client and the target.
	log.Debug("proxy connected to target, piping data")
	go func() {
		io.Copy(targetConn, clientConn)
	}()
	io.Copy(clientConn, targetConn)
}

// handleNonConnect forwards non-CONNECT requests to the proxy server with the required headers.
func (h *proxyHandler) handleNonConnect(proxyResp http.ResponseWriter, proxyReq *http.Request) {
	// To avoid modifying the original request, we create a new identical request that we give to
	// the http client to modify as needed. The result is then copied to the original response writer.
	targetReq, err := http.NewRequestWithContext(
		proxyReq.Context(),
		proxyReq.Method,
		proxyReq.URL.String(),
		proxyReq.Body,
	)
	if err != nil {
		sendError(proxyResp, "Error creating target request", http.StatusInternalServerError, err)
		return
	}
	targetReq.Header = proxyReq.Header.Clone()
	addRequiredHeaders(targetReq, h.authToken)

	targetResp, err := h.client.Do(targetReq)
	if err != nil {
		sendError(proxyResp, "Failed to fetch destination", http.StatusServiceUnavailable, err)
		return
	}
	defer targetResp.Body.Close()

	for key, values := range targetResp.Header {
		for _, value := range values {
			proxyResp.Header().Add(key, value)
		}
	}
	_, err = io.Copy(proxyResp, targetResp.Body)
	if err != nil {
		sendError(proxyResp, "Failed write response", http.StatusServiceUnavailable, err)
	}
}

// addRequiredHeaders adds the required headers to the request. Currently, all but the auth token are placeholders.
func addRequiredHeaders(req *http.Request, authToken string) {
	req.Header.Set(appVersionHeader, clientVersion)
	req.Header.Set(versionHeader, version)
	req.Header.Set(userIdHeader, userId)
	req.Header.Set(platformHeader, "linux")
	req.Header.Set(appNameHeader, "radiance")
	req.Header.Set(deviceIdHeader, "some-uuid-here")
	req.Header.Set(authTokenHeader, authToken)
}

// sendError is a helper function to log an error and send an error message to the client.
func sendError(resp http.ResponseWriter, msg string, status int, err error) {
	log.Errorf("%v: %v", msg, err)
	http.Error(resp, msg, status)
}
