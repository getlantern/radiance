package vpn

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

const authTokenHeader = "X-Lantern-Auth-Token"

type connectDialer struct {
	innerSD   transport.StreamDialer
	proxyAddr string
	authToken string
}

func newConnectDialer(innerSD transport.StreamDialer, proxyAddr, authToken string) transport.StreamDialer {
	return &connectDialer{
		innerSD:   innerSD,
		proxyAddr: proxyAddr,
		authToken: authToken,
	}
}

func (sd *connectDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	conn, err := sd.innerSD.DialStream(ctx, sd.proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to dial target: %w", err)
	}

	if err := shakeHand(conn, addr, sd.authToken); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to shake hands with proxy: %w", err)
	}
	return conn, nil
}

func shakeHand(conn net.Conn, remoteAddr, authToken string) error {
	_, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		remoteAddr = net.JoinHostPort(remoteAddr, "80")
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL: &url.URL{
			Host: remoteAddr,
		},
		Host: remoteAddr,
		Header: http.Header{
			authTokenHeader:    []string{authToken},
			"Proxy-Connection": []string{"Keep-Alive"},
		},
	}
	log.Debugf("sending CONNECT request for %s", req.Host)
	if err := req.Write(conn); err != nil {
		return err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return fmt.Errorf("failed to read CONNECT response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CONNECT request failed: %s", resp.Status)
	}
	log.Debugf("CONNECT request for %s successful", remoteAddr)
	return nil
}
