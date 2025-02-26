package radiance

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/transport/proxyless"
)

const authTokenHeader = "X-Lantern-Auth-Token"

// proxyHandler sends all requests over dialer to the proxy server. Requests are handled differently
// based on whether they are CONNECT requests or not. CONNECT requests, which are usually https, will
// establish a tunnel to the target server. All other requests are forwarded to the proxy server to
// let it decide how to handle them.
type proxyHandler struct {
	// addr is the address of the proxy server.
	addr string
	// authToken is the authentication token to send with each request to the proxy server.
	authToken string
	dialer    transport.StreamDialer
	// client is an http client that will be used to forward non-CONNECT requests to the proxy server.
	client          http.Client
	proxylessDialer transport.StreamDialer
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
	// We're responsible for closing the client connection since we hijacked it. But since we're
	// just piping data back and forth, we can give that responsibility back to the server, which
	// will close the connection when the request is done.
	context.AfterFunc(proxyReq.Context(), func() { clientConn.Close() })
	log.Debug("hijacked connection")

	if h.proxylessDialer != nil {
		err = h.tryProxylessConnect(proxyReq, clientConn)
		if err == nil {
			return
		}
		log.Debugf("failed with proxyless connection: %w", err)
	}

	targetConn, err := h.dialer.DialStream(proxyReq.Context(), h.addr)
	if err != nil {
		sendError(proxyResp, "Failed to dial target", http.StatusServiceUnavailable, err)
		return
	}
	defer targetConn.Close()

	// Create a new CONNECT request to send to the proxy server.
	connectReq, err := backend.NewRequestWithHeaders(
		proxyReq.Context(),
		http.MethodConnect,
		proxyReq.URL.String(),
		http.NoBody,
	)
	if err != nil {
		sendError(proxyResp, "Error creating connect request", http.StatusInternalServerError, err)
		return
	}
	connectReq.Header.Set(authTokenHeader, h.authToken)

	if err = connectReq.Write(targetConn); err != nil {
		sendError(proxyResp, "Failed to write connect request to proxy", http.StatusInternalServerError, err)
		return
	}
	if err = h.pipeData(targetConn, clientConn); err != nil && !errors.Is(err, io.EOF) {
		log.Errorf("failed to pipe data between target and client connections: %w", err)
	}
}

func (h *proxyHandler) pipeData(targetConn net.Conn, clientConn net.Conn) error {
	// Pipe data between the client and the target.
	log.Debug("proxy connected to target, piping data")
	errCh := make(chan error, 2)
	go copyData(targetConn, clientConn, errCh)
	go copyData(clientConn, targetConn, errCh)

	return <-errCh
}

func copyData(dst io.Writer, src io.Reader, errCh chan error) {
	_, err := io.Copy(dst, src)
	errCh <- err
}

func (h *proxyHandler) tryProxylessConnect(r *http.Request, clientConn net.Conn) error {
	ctx := context.WithValue(r.Context(), proxyless.DomainContextKey, r.Host)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	raddr, err := raddrFromRequest(r)
	if err != nil {
		return err
	}

	targetConn, err := h.proxylessDialer.DialStream(ctx, raddr)
	if err != nil {
		return log.Errorf("failed to proxyless dial: %w. Trying with proxy instead", err)
	}
	defer targetConn.Close()

	// if proxyless succeed, we can return a 200 response that indicates we're able to
	// stream the request and response
	_, err = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if err != nil {
		return log.Errorf("failed to send connection established: %w", err)
	}

	return h.pipeData(targetConn, clientConn)
}

func raddrFromRequest(r *http.Request) (string, error) {
	ips, err := net.LookupIP(r.Host)
	if err != nil {
		return "", log.Errorf("failed to lookup for host IPs: %w", err)
	}

	port := "80"
	if r.TLS != nil {
		port = "443"
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}

// handleNonConnect forwards non-CONNECT requests to the proxy server with the required headers.
func (h *proxyHandler) handleNonConnect(proxyResp http.ResponseWriter, proxyReq *http.Request) {
	// To avoid modifying the original request, we create a new identical request that we give to
	// the http client to modify as needed. The result is then copied to the original response writer.
	targetReq, err := backend.NewRequestWithHeaders(
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
	targetReq.Header.Set(authTokenHeader, h.authToken)
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

// sendError is a helper function to log an error and send an error message to the client.
func sendError(resp http.ResponseWriter, msg string, status int, err error) {
	log.Errorf("%v: %v", msg, err)
	http.Error(resp, msg, status)
}
