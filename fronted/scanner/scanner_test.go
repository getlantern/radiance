package scanner

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/getlantern/domainfront"
	tls "github.com/refraction-networking/utls"
	stdtls "crypto/tls"
)

func TestProbe_SuccessfulHandshakeAnd200(t *testing.T) {
	srv, ca := newTLSEchoServer(t, "test.example", http.StatusOK)
	t.Cleanup(srv.Close)

	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	c := Candidate{
		Provider:       "test",
		Domain:         "test.example",
		IPAddress:      host + ":" + port,
		TestURL:        fmt.Sprintf("https://%s/ping", srv.Listener.Addr().String()),
		InnerHost:      "test.example",
		VerifyHostname: "test.example",
	}

	res := Probe(context.Background(), c, Options{RootCAs: ca, DialTimeout: 2 * time.Second})

	if !res.OK() {
		t.Fatalf("probe failed: status=%d err=%v", res.Status, res.Err)
	}
	if res.Latency <= 0 {
		t.Errorf("latency = %v; want > 0", res.Latency)
	}
}

func TestProbe_TCPConnectFails(t *testing.T) {
	c := Candidate{
		Provider:  "test",
		Domain:    "test.example",
		IPAddress: "127.0.0.1:1",
		TestURL:   "https://test.example/ping",
		InnerHost: "test.example",
	}
	res := Probe(context.Background(), c, Options{DialTimeout: 500 * time.Millisecond})
	if res.OK() {
		t.Errorf("expected failure, got OK result")
	}
	if res.Err == nil {
		t.Errorf("expected non-nil error")
	}
}

func TestProbe_TLSWrongHostname(t *testing.T) {
	srv, ca := newTLSEchoServer(t, "test.example", http.StatusOK)
	t.Cleanup(srv.Close)

	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := Candidate{
		Provider:       "test",
		Domain:         "wrong.example",
		IPAddress:      host + ":" + port,
		TestURL:        fmt.Sprintf("https://%s/ping", srv.Listener.Addr().String()),
		VerifyHostname: "wrong.example",
	}
	res := Probe(context.Background(), c, Options{RootCAs: ca, DialTimeout: 2 * time.Second})
	if res.OK() {
		t.Errorf("expected hostname mismatch failure, got OK")
	}
}

func TestProbe_HTTP500NotOK(t *testing.T) {
	srv, ca := newTLSEchoServer(t, "test.example", http.StatusInternalServerError)
	t.Cleanup(srv.Close)

	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := Candidate{
		Provider:       "test",
		Domain:         "test.example",
		IPAddress:      host + ":" + port,
		TestURL:        fmt.Sprintf("https://%s/ping", srv.Listener.Addr().String()),
		VerifyHostname: "test.example",
	}
	res := Probe(context.Background(), c, Options{RootCAs: ca, DialTimeout: 2 * time.Second})
	if res.OK() {
		t.Errorf("expected 5xx to fail OK()")
	}
	if res.Status != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", res.Status)
	}
}

func TestScan_RanksByLatency(t *testing.T) {
	srvFast, ca := newTLSEchoServer(t, "fast.example", http.StatusOK)
	t.Cleanup(srvFast.Close)
	srvSlow, _ := newTLSEchoServerWithCA(t, "slow.example", http.StatusOK, ca, 100*time.Millisecond)
	t.Cleanup(srvSlow.Close)

	cands := []Candidate{
		{
			Provider:       "test",
			Domain:         "slow.example",
			IPAddress:      srvSlow.Listener.Addr().String(),
			TestURL:        fmt.Sprintf("https://%s/ping", srvSlow.Listener.Addr().String()),
			VerifyHostname: "slow.example",
		},
		{
			Provider:       "test",
			Domain:         "fast.example",
			IPAddress:      srvFast.Listener.Addr().String(),
			TestURL:        fmt.Sprintf("https://%s/ping", srvFast.Listener.Addr().String()),
			VerifyHostname: "fast.example",
		},
		{
			Provider:  "test",
			Domain:    "deadend.example",
			IPAddress: "127.0.0.1:1",
			TestURL:   "https://deadend.example/ping",
		},
	}

	results := Scan(context.Background(), cands, Options{RootCAs: ca, DialTimeout: 3 * time.Second})
	if len(results) != 3 {
		t.Fatalf("len(results) = %d; want 3", len(results))
	}

	ranked := RankWorking(results)
	if len(ranked) != 2 {
		t.Fatalf("RankWorking returned %d; want 2", len(ranked))
	}
	if ranked[0].Candidate.Domain != "fast.example" {
		t.Errorf("rank[0] = %q; want fast.example", ranked[0].Candidate.Domain)
	}
	if ranked[1].Candidate.Domain != "slow.example" {
		t.Errorf("rank[1] = %q; want slow.example", ranked[1].Candidate.Domain)
	}
}

func TestCandidatesFromConfig(t *testing.T) {
	cfg := &domainfront.Config{
		Providers: map[string]*domainfront.Provider{
			"akamai": {
				TestURL: "https://fronted-ping.dsa.akamai.getiantem.org/ping",
				Masquerades: []*domainfront.Masquerade{
					{Domain: "a248.e.akamai.net", IpAddress: "23.47.48.230"},
					{Domain: "a248.e.akamai.net", IpAddress: "184.150.49.62"},
				},
			},
			"cloudfront": {
				TestURL: "http://d157vud77ygy87.cloudfront.net/ping",
				Masquerades: []*domainfront.Masquerade{
					{Domain: "aa1.awsstatic.com", IpAddress: "99.84.2.4"},
				},
			},
		},
	}

	cands, err := CandidatesFromConfig(cfg)
	if err != nil {
		t.Fatalf("CandidatesFromConfig: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("len(cands) = %d; want 3", len(cands))
	}

	byProvider := map[string]int{}
	for _, c := range cands {
		byProvider[c.Provider]++
		if c.InnerHost == "" {
			t.Errorf("candidate %v has empty InnerHost", c)
		}
		if c.TestURL == "" {
			t.Errorf("candidate %v has empty TestURL", c)
		}
	}
	if byProvider["akamai"] != 2 || byProvider["cloudfront"] != 1 {
		t.Errorf("provider distribution wrong: %v", byProvider)
	}
}

func TestCandidatesFromConfig_NilConfigReturnsError(t *testing.T) {
	_, err := CandidatesFromConfig(nil)
	if err == nil {
		t.Errorf("expected error for nil config")
	}
}

func TestSNIsForProvider_UniqueAndOrdered(t *testing.T) {
	cfg := &domainfront.Config{
		Providers: map[string]*domainfront.Provider{
			"cloudfront": {
				Masquerades: []*domainfront.Masquerade{
					{Domain: "aa1.awsstatic.com", IpAddress: "1.1.1.1"},
					{Domain: "aa1.awsstatic.com", IpAddress: "1.1.1.2"}, // dup
					{Domain: "advertising.amazon.com", IpAddress: "2.2.2.2"},
					{Domain: "", IpAddress: "3.3.3.3"}, // skip empty
				},
			},
		},
	}
	got := SNIsForProvider(cfg, "cloudfront")
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	want := map[string]bool{"aa1.awsstatic.com": true, "advertising.amazon.com": true}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected SNI %q", s)
		}
	}
}

func TestSNIsForProvider_MissingProvider(t *testing.T) {
	cfg := &domainfront.Config{Providers: map[string]*domainfront.Provider{}}
	if got := SNIsForProvider(cfg, "cloudfront"); got != nil {
		t.Errorf("missing provider should yield nil, got %v", got)
	}
}

// --- helpers ---

func newTLSEchoServer(t *testing.T, dnsName string, status int) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSignedCert(t, dnsName)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &stdtls.Config{Certificates: []stdtls.Certificate{cert}}
	srv.StartTLS()
	return srv, pool
}

func newTLSEchoServerWithCA(t *testing.T, dnsName string, status int, pool *x509.CertPool, delay time.Duration) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	cert, _ := selfSignedCertWithPool(t, dnsName, pool)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = &stdtls.Config{Certificates: []stdtls.Certificate{cert}}
	srv.StartTLS()
	return srv, pool
}

func selfSignedCert(t *testing.T, dnsName string) (stdtls.Certificate, *x509.CertPool) {
	t.Helper()
	return selfSignedCertWithPool(t, dnsName, x509.NewCertPool())
}

func selfSignedCertWithPool(t *testing.T, dnsName string, pool *x509.CertPool) (stdtls.Certificate, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: dnsName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{dnsName},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("createcert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsecert: %v", err)
	}
	pool.AddCert(cert)

	tlsCert := stdtls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        cert,
	}
	return tlsCert, pool
}

// Used by tls.HelloGolang in tests — guards against the utls import being
// hidden and the build succeeding by accident on a stdlib-tls fallback.
var _ = pem.Decode
var _ = tls.HelloGolang
var _ = errors.New
