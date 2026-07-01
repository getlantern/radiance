// Command geosite-throttle-test checks whether TLS fragmentation lets a full
// geosite-cn.srs download complete from a real CN residential IP — i.e. whether
// the GFW throttle that kills the plain `direct` fetch is SNI-triggered (and so
// defeated by fragmenting the ClientHello) or IP/flow-based. This validates the
// "mirror" outbound approach (engineering#3657) before merge.
//
// It routes the outline-sdk fragmentation dialers' inner dial through an oxylabs
// CN-residential HTTP CONNECT exit, then fetches each candidate .srs over HTTPS
// with each strategy and reports bytes/time. `direct` is the control (expected
// to stall, matching the earlier curl finding); `tlsfrag` is the experiment.
//
// NOTE on method: tlsfrag is *TLS-record* fragmentation — it survives the
// CONNECT proxy (record boundaries are application-layer). `split` is *TCP-write*
// splitting, which a CONNECT proxy may coalesce, so a split failure here is not
// conclusive; a tlsfrag result IS.
//
// Run: source the residential-proxy skill's residential-proxy-env.sh first
// (exports OXY_USER/OXY_PASS), then: go run ./cmd/geosite-throttle-test
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/split"
	"github.com/Jigsaw-Code/outline-sdk/transport/tlsfrag"
)

const (
	oxyGateway = "pr.oxylabs.io:7777"
	oxyCC      = "cn"
)

// connectDialer tunnels through the oxylabs HTTP CONNECT proxy so the TCP
// connection to the target originates from a CN residential exit. Resolution
// happens at the exit (CONNECT carries the hostname).
type connectDialer struct{ user, pass string }

func (d connectDialer) DialStream(ctx context.Context, addr string) (transport.StreamConn, error) {
	c, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", oxyGateway)
	if err != nil {
		return nil, fmt.Errorf("dial proxy: %w", err)
	}
	tcp := c.(*net.TCPConn)
	auth := base64.StdEncoding.EncodeToString([]byte(d.user + ":" + d.pass))
	fmt.Fprintf(tcp, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", addr, addr, auth)
	br := bufio.NewReader(tcp)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		tcp.Close()
		return nil, fmt.Errorf("read CONNECT resp: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tcp.Close()
		return nil, fmt.Errorf("CONNECT failed: %s", resp.Status)
	}
	return &bufConn{TCPConn: tcp, r: br}, nil
}

// bufConn preserves any bytes the reader buffered past the CONNECT response.
type bufConn struct {
	*net.TCPConn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func sessUser(base string) string {
	return fmt.Sprintf("customer-%s-cc-%s-sessid-%d-sesstime-10", base, oxyCC, time.Now().UnixNano()%1000000)
}

func fetch(dialer transport.StreamDialer, url string) (int64, time.Duration, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialStream(ctx, addr)
		},
		TLSHandshakeTimeout: 15 * time.Second,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSClientConfig:     &tls.Config{NextProtos: []string{"http/1.1"}},
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, resp.Body)
	return n, time.Since(start), err
}

func main() {
	oxyUser, oxyPass := os.Getenv("OXY_USER"), os.Getenv("OXY_PASS")
	if oxyUser == "" || oxyPass == "" {
		fmt.Fprintln(os.Stderr, "set OXY_USER/OXY_PASS (source the residential-proxy-env.sh first)")
		os.Exit(1)
	}

	targets := []struct {
		name, url string
		size      int64
	}{
		{"fastly", "https://fastly.jsdelivr.net/gh/MetaCubeX/meta-rules-dat@sing/geo/geosite/cn.srs", 0},
		{"raw.githubusercontent", "https://raw.githubusercontent.com/getlantern/rulesets/681577d4c825397df1521337f90ea5d3a3383c77/srs/geosite-cn.srs", 454323},
	}

	strats := []struct {
		name string
		wrap func(transport.StreamDialer) (transport.StreamDialer, error)
	}{
		{"direct", func(b transport.StreamDialer) (transport.StreamDialer, error) { return b, nil }},
		{"tlsfrag:1", func(b transport.StreamDialer) (transport.StreamDialer, error) { return tlsfrag.NewFixedLenStreamDialer(b, 1) }},
		{"tlsfrag:5", func(b transport.StreamDialer) (transport.StreamDialer, error) { return tlsfrag.NewFixedLenStreamDialer(b, 5) }},
		{"split:1", func(b transport.StreamDialer) (transport.StreamDialer, error) {
			return split.NewStreamDialer(b, split.NewFixedSplitIterator(1))
		}},
	}

	for _, t := range targets {
		fmt.Printf("\n===== %s — %s =====\n", t.name, t.url)
		for _, s := range strats {
			for i := 1; i <= 2; i++ {
				base := connectDialer{user: sessUser(oxyUser), pass: oxyPass}
				d, err := s.wrap(base)
				if err != nil {
					fmt.Printf("  %-10s #%d  build-err: %v\n", s.name, i, err)
					continue
				}
				n, dur, err := fetch(d, t.url)
				status := "OK"
				switch {
				case err != nil:
					status = "ERR: " + err.Error()
				case t.size > 0 && n < t.size:
					status = fmt.Sprintf("PARTIAL (%d/%d)", n, t.size)
				}
				fmt.Printf("  %-10s #%d  bytes=%-7d time=%-8s %s\n", s.name, i, n, dur.Round(time.Millisecond), status)
			}
		}
	}
}
