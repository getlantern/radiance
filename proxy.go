package radiance

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/Jigsaw-Code/outline-sdk/transport"

	"github.com/getlantern/radiance/backend"
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
		if err := h.handleProxylessConnect(resp, req); err != nil {
			log.Debugf("failed to handle proxyless connect: %w", err)
			h.handleConnect(resp, req)
		}
		return
	}
	h.handleNonConnect(resp, req)
}

func (h *proxyHandler) handleProxylessConnect(w http.ResponseWriter, r *http.Request) error {
	if h.proxylessDialer == nil {
		return fmt.Errorf("proxyless dialer not defined")
	}
	targetConn, err := h.proxylessDialer.DialStream(r.Context(), r.Host)
	if err != nil {
		return log.Errorf("failed to proxyless dial target: %w", err)
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return log.Errorf("request doesn't support hijacking")
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return log.Errorf("failed to hijack connection: %w", err)
	}
	context.AfterFunc(r.Context(), func() { clientConn.Close() })

	connectReq, err := http.NewRequestWithContext(r.Context(), http.MethodConnect, r.URL.String(), http.NoBody)
	if err != nil {
		return log.Errorf("failed to create connect request: %w", err)
	}
	if err = connectReq.Write(targetConn); err != nil {
		return log.Errorf("failed to write connect request to proxy: %w", err)
	}
	// Pipe data between the client and the target.
	log.Debug("proxy connected to target, piping data")
	go func() {
		_, err := io.Copy(targetConn, clientConn)
		if err != nil {
			log.Errorf("Failed to copy data to target: %v", err)
		}
	}()
	_, err = io.Copy(clientConn, targetConn)
	if err != nil {
		log.Errorf("Failed to copy data to client: %v", err)
	}
	return nil
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

	var connectReq *http.Request
	// Create a new CONNECT request to send to the proxy server.
	connectReq, err = backend.NewRequestWithHeaders(
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

	// Pipe data between the client and the target.
	log.Debug("proxy connected to target, piping data")
	go func() {
		_, err := io.Copy(targetConn, clientConn)
		if err != nil {
			log.Errorf("Failed to copy data to target: %v", err)
		}
	}()
	_, err = io.Copy(clientConn, targetConn)
	if err != nil {
		log.Errorf("Failed to copy data to client: %v", err)
	}
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
