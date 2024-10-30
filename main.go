package radiance

/*
	TODO:
		- use outline transports where possible
		- other transports need to implement the StreamDialer interface
		- local http proxy (testing)
		- add logging

		PR-1:
			- read proxy config from a file
			- use first shadowsocks or tls proxy
			- connect to proxy using outline transport

		PR-2:
			- retrieve proxy config from backend

		PR-3:
			- update transports not implemented in outline to implement StreamDialer interface
			(maybe break this into multiple PRs)

		PR-4:
			- add socks5 support from outline

		PR-5:
			- implement vpn TUN

		PR-6:
			- doc
*/

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/getlantern/radiance/transport"
)

func run(proxyConfig, addr string) error {
	dialer, err := transport.DialerFrom(proxyConfig)
	if err != nil {
		return fmt.Errorf("Could not create dialer: %v", err)
	}

	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid addr: %v", err)
		}

		if !strings.HasPrefix(network, "tcp") {
			return nil, fmt.Errorf("protocol not supported: %v", network)
		}

		return dialer.DialStream(ctx, net.JoinHostPort(host, port))
	}

	httpClient := &http.Client{
		Transport: &http.Transport{DialContext: dialContext},
		Timeout:   time.Duration(time.Second * 10),
	}

	resp, err := httpClient.Head(addr)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	var headers string
	for k, v := range resp.Header {
		headers += fmt.Sprintf("%s: %s\n", k, v)
	}

	log.Debugf("Response:\n%v\n", headers)

	return nil
}

func doRequest(client *http.Client, addr string) error {
	resp, err := client.Head(addr)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	var headers string
	for k, v := range resp.Header {
		headers += fmt.Sprintf("%s: %s\n", k, v)
	}

	log.Debugf("Response:\n%v\n", headers)
	return nil
}

func main() {
	rad, _ := New()
	if err := rad.Run("localhost:8080"); err != nil {
		log.Fatalf("Failed to run radiance: %v", err)
	}

	// Wait for interrupt signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	log.Debug("Shutting down radiance")
	if err := rad.Shutdown(); err != nil {
		log.Fatalf("Failed to shutdown radiance: %v", err)
	}
}
