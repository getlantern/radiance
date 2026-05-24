// Command meek-client-smoke exercises the lantern-box meek client
// against the deployed meek-test server via the radiance fronted
// scanner. Prints httpbin's reported origin — equality with the
// known Linode IP proves traffic completed the full client→Akamai
// →Caddy→meek-server→microsocks→internet round trip.
//
// Run:
//   go run ./cmd/meek-client-smoke
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	lbmeek "github.com/getlantern/lantern-box/protocol/meek"

	"github.com/getlantern/radiance/kindling/fronted"
	rmeek "github.com/getlantern/radiance/kindling/meek"
)

const (
	meekURL       = "https://meek.dsa.akamai.getiantem.org/"
	targetHost    = "httpbin.org"
	targetPort    = 80
	expectedOrigin = "139.162.181.47" // Linode public IP
)

func main() {
	if err := run(); err != nil {
		slog.Error("test failed", "err", err)
		os.Exit(1)
	}
	fmt.Println("\n✅ end-to-end meek client smoke test PASSED")
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slog.Info("step 1: load fronted config + start scanner")
	dataDir, err := os.MkdirTemp("", "meek-client-smoke-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(dataDir)

	cfg, err := fronted.LoadCachedConfig(dataDir)
	if err != nil {
		return fmt.Errorf("load fronted config: %w", err)
	}

	// Only sample Akamai fronts — our meek property is on Akamai, so
	// CloudFront IPs would dial a CDN that doesn't host the meek server
	// and the poll-response loop would hang on miss-routed requests.
	provider, err := rmeek.NewProvider(rmeek.ProviderConfig{
		Config:           cfg,
		CacheFile:        filepath.Join(dataDir, "meek_fronts_cache.json"),
		KnownSample:      0,
		CloudFrontSample: 0,
		AkamaiSample:     50,
	})
	if err != nil {
		return fmt.Errorf("new provider: %w", err)
	}
	defer provider.Close()
	provider.Start(ctx)

	slog.Info("step 2: wait up to 30s for scanner to find working Akamai fronts")
	var fronts []rmeek.FrontSpec
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		fronts = provider.FrontSpecs(3)
		if len(fronts) > 0 {
			break
		}
	}
	if len(fronts) == 0 {
		return errors.New("scanner found no working fronts in 30s")
	}
	slog.Info("got fronts", "count", len(fronts), "first_ip", fronts[0].IPAddress, "first_sni", fronts[0].SNI)

	slog.Info("step 3: build HTTPClient and dial meek server")
	u, err := url.Parse(meekURL)
	if err != nil {
		return fmt.Errorf("parse meek url: %w", err)
	}
	httpClient := buildFrontedHTTPClient(fronts, 10*time.Second)

	conn, err := lbmeek.Dial(ctx, lbmeek.Config{
		URL:          meekURL,
		InnerHost:    u.Host,
		HTTPClient:   httpClient,
		PollInterval: 100 * time.Millisecond,
		ReadTimeout:  30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("meek dial: %w", err)
	}
	defer conn.Close()

	slog.Info("step 4: SOCKS5 method-select")
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("write method-select: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("read method-select reply: %w", err)
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("method-select reply unexpected: %x", resp)
	}
	slog.Info("✅ SOCKS5 NO_AUTH accepted", "reply", fmt.Sprintf("%x", resp))

	slog.Info("step 5: SOCKS5 CONNECT", "target", fmt.Sprintf("%s:%d", targetHost, targetPort))
	connectReq := []byte{0x05, 0x01, 0x00, 0x03, byte(len(targetHost))}
	connectReq = append(connectReq, []byte(targetHost)...)
	connectReq = append(connectReq, byte(targetPort>>8), byte(targetPort&0xff))
	if _, err := conn.Write(connectReq); err != nil {
		return fmt.Errorf("write CONNECT: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	connectReply := make([]byte, 10)
	if _, err := io.ReadFull(conn, connectReply); err != nil {
		return fmt.Errorf("read CONNECT reply: %w", err)
	}
	if connectReply[0] != 0x05 || connectReply[1] != 0x00 {
		return fmt.Errorf("CONNECT reply unexpected: %x", connectReply)
	}
	slog.Info("✅ SOCKS5 CONNECT succeeded", "reply", fmt.Sprintf("%x", connectReply))

	slog.Info("step 6: HTTP GET /ip")
	httpReq := fmt.Sprintf("GET /ip HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n", targetHost)
	if _, err := conn.Write([]byte(httpReq)); err != nil {
		return fmt.Errorf("write HTTP request: %w", err)
	}
	conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	var bodyBuf strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			bodyBuf.Write(buf[:n])
			if strings.Contains(bodyBuf.String(), expectedOrigin+"\"") {
				break
			}
		}
		if err != nil {
			break
		}
	}
	body := bodyBuf.String()
	slog.Info("✅ HTTP response received", "bytes", len(body))
	fmt.Println("--- HTTP response ---")
	fmt.Println(body)

	if !strings.Contains(body, expectedOrigin) {
		return fmt.Errorf("expected origin %q not in response body", expectedOrigin)
	}
	return nil
}

// buildFrontedHTTPClient returns a client that dials a random front by IP
// and validates the served chain against the front's VerifyHostname rather
// than the request URL's host.
func buildFrontedHTTPClient(fronts []rmeek.FrontSpec, connectTimeout time.Duration) *http.Client {
	if len(fronts) == 0 {
		panic("buildFrontedHTTPClient: no fronts")
	}
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			front := fronts[time.Now().UnixNano()%int64(len(fronts))]
			addr := front.IPAddress
			if !strings.Contains(addr, ":") {
				addr = net.JoinHostPort(addr, "443")
			}
			dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()
			d := &net.Dialer{}
			raw, err := d.DialContext(dialCtx, "tcp", addr)
			if err != nil {
				return nil, fmt.Errorf("tcp dial %s: %w", addr, err)
			}
			tlsCfg := &tls.Config{InsecureSkipVerify: true}
			if front.SNI != "" {
				tlsCfg.ServerName = front.SNI
			}
			verifyHost := front.VerifyHostname
			if verifyHost == "" {
				verifyHost = front.SNI
			}
			tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				return verifyChain(rawCerts, verifyHost)
			}
			conn := tls.Client(raw, tlsCfg)
			if err := conn.HandshakeContext(dialCtx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("tls handshake: %w", err)
			}
			return conn, nil
		},
		DisableKeepAlives: false,
		IdleConnTimeout:   90 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 60 * time.Second}
}

func verifyChain(rawCerts [][]byte, verifyHost string) error {
	if len(rawCerts) == 0 {
		return errors.New("no peer certs")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("parse cert: %w", err)
		}
		certs = append(certs, c)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("system cert pool: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	_, err = certs[0].Verify(x509.VerifyOptions{
		DNSName:       verifyHost,
		Roots:         roots,
		Intermediates: intermediates,
	})
	return err
}
