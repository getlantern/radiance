// Package scanner probes domain-fronting candidates from the user's
// network position to find which routes actually work end-to-end.
//
// A successful probe is a TCP+TLS handshake to a CDN edge IP using the
// candidate's outer SNI followed by an HTTPS GET to TestURL with
// InnerHost as the Host header that returns 2xx. Both legs must work:
// TLS-only success would only confirm the edge is reachable, not that
// the inner Host routes to our backend through that edge.
//
// The scanner is intended to run client-side so each user's results
// reflect their ISP, geography, and time of day — the variables Samim
// Mirhosseini identified as load-bearing for IR-specific fronting.
package scanner

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
)

// Candidate describes one (CDN edge, masquerade) pair to probe.
//
// SNI semantics matter and differ between providers. Empty SNI means
// "send no SNI extension at all" — Akamai edges return their default
// cert in that mode, which validates against Domain. Non-empty SNI is
// sent in the ClientHello — CloudFront edges serve cert content keyed
// to the SNI value, so the masquerade domain must be passed in SNI.
// Match the production domainfront dialer's behavior: leave SNI empty
// for Akamai-style entries; set it explicitly for CloudFront-style
// entries.
//
// Domain identifies the logical front and is the hostname the
// post-handshake cert chain is verified against when VerifyHostname
// isn't overridden.
type Candidate struct {
	Provider       string
	Domain         string
	IPAddress      string
	SNI            string
	VerifyHostname string
	TestURL        string
	InnerHost      string
}

func (c Candidate) outerSNI() string {
	return c.SNI
}

func (c Candidate) verify() string {
	if c.VerifyHostname != "" {
		return c.VerifyHostname
	}
	return c.Domain
}

type Result struct {
	Candidate Candidate
	Latency   time.Duration
	Status    int
	Err       error
}

func (r Result) OK() bool { return r.Err == nil && r.Status >= 200 && r.Status < 300 }

type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

type Options struct {
	Dialer        Dialer
	RootCAs       *x509.CertPool
	ClientHelloID tls.ClientHelloID
	DialTimeout   time.Duration
	Concurrency   int
	// OnResult, when set, is called once per probe as it completes,
	// from the probing goroutine. Multiple goroutines invoke it
	// concurrently, so it must be safe for concurrent use. Lets callers
	// consume working fronts as they're found rather than waiting for
	// the whole scan.
	OnResult func(Result)
}

func (o *Options) defaults() {
	if o.Dialer == nil {
		o.Dialer = &net.Dialer{}
	}
	if o.ClientHelloID.Client == "" {
		o.ClientHelloID = tls.HelloGolang
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 8
	}
}

var errNoTestURL = errors.New("candidate has no TestURL")

func Probe(ctx context.Context, c Candidate, opts Options) Result {
	opts.defaults()

	start := time.Now()
	res := Result{Candidate: c}

	if c.TestURL == "" {
		res.Err = errNoTestURL
		return res
	}
	if c.IPAddress == "" {
		res.Err = errors.New("candidate has no IPAddress")
		return res
	}

	addr := c.IPAddress
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "443")
	}

	dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()

	rawConn, err := opts.Dialer.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		res.Latency = time.Since(start)
		res.Err = fmt.Errorf("tcp: %w", err)
		return res
	}

	deadline := time.Now().Add(opts.DialTimeout)
	_ = rawConn.SetDeadline(deadline)

	verifyHost := c.verify()
	tlsConfig := &tls.Config{
		RootCAs:            opts.RootCAs,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyCertChain(rawCerts, opts.RootCAs, verifyHost)
		},
	}
	if outer := c.outerSNI(); outer != "" {
		tlsConfig.ServerName = outer
	}

	tlsConn := tls.UClient(rawConn, tlsConfig, opts.ClientHelloID)
	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		rawConn.Close()
		res.Latency = time.Since(start)
		res.Err = fmt.Errorf("tls: %w", err)
		return res
	}

	req, err := buildProbeRequest(dialCtx, c)
	if err != nil {
		tlsConn.Close()
		res.Latency = time.Since(start)
		res.Err = err
		return res
	}

	tr := &http.Transport{
		DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
			return tlsConn, nil
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: tr, Timeout: opts.DialTimeout}

	resp, err := client.Do(req)
	if err != nil {
		tlsConn.Close()
		res.Latency = time.Since(start)
		res.Err = fmt.Errorf("http: %w", err)
		return res
	}
	defer resp.Body.Close()
	tr.CloseIdleConnections()

	res.Status = resp.StatusCode
	res.Latency = time.Since(start)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.Err = fmt.Errorf("http status %d", resp.StatusCode)
	}
	return res
}

func buildProbeRequest(ctx context.Context, c Candidate) (*http.Request, error) {
	u, err := url.Parse(c.TestURL)
	if err != nil {
		return nil, fmt.Errorf("parse TestURL: %w", err)
	}
	// The outer connection is TLS on port 443 regardless of the
	// TestURL scheme. http.Transport only routes via DialTLSContext
	// (our pre-opened fronted TLS conn) for https URLs — if scheme
	// stays http the request falls through to plain-text DNS + port
	// 80, bypassing the front entirely. Some providers' testurls
	// (CloudFront in fronted.yaml.gz) ship as http://.
	u.Scheme = "https"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if c.InnerHost != "" {
		req.Host = c.InnerHost
	}
	return req, nil
}

// Scan probes candidates concurrently and returns one Result per
// candidate. Results retain input order so callers can correlate by
// index; sort by Latency or filter by OK() to rank.
func Scan(ctx context.Context, candidates []Candidate, opts Options) []Result {
	opts.defaults()

	results := make([]Result, len(candidates))
	if len(candidates) == 0 {
		return results
	}

	sem := make(chan struct{}, opts.Concurrency)
	var wg sync.WaitGroup
	for i, c := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c Candidate) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := ctx.Err(); err != nil {
				results[i] = Result{Candidate: c, Err: err}
				return
			}
			results[i] = Probe(ctx, c, opts)
			if opts.OnResult != nil {
				opts.OnResult(results[i])
			}
		}(i, c)
	}
	wg.Wait()
	return results
}

// RankWorking returns the OK() results sorted by latency ascending.
func RankWorking(results []Result) []Result {
	out := make([]Result, 0, len(results))
	for _, r := range results {
		if r.OK() {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Latency < out[j].Latency })
	return out
}

func verifyCertChain(rawCerts [][]byte, roots *x509.CertPool, dnsName string) error {
	if len(rawCerts) == 0 {
		return errors.New("no certificates presented")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return fmt.Errorf("parse leaf: %w", err)
	}
	opts := x509.VerifyOptions{
		Roots:         roots,
		CurrentTime:   time.Now(),
		DNSName:       dnsName,
		Intermediates: x509.NewCertPool(),
	}
	for i := 1; i < len(rawCerts); i++ {
		c, err := x509.ParseCertificate(rawCerts[i])
		if err != nil {
			return fmt.Errorf("parse intermediate %d: %w", i, err)
		}
		opts.Intermediates.AddCert(c)
	}
	if _, err := cert.Verify(opts); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}
