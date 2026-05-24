package scanner

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"

	"github.com/getlantern/domainfront"
)

// CandidatesFromConfig flattens a parsed domainfront config into a probe
// list. Each (provider, masquerade) pair becomes one Candidate; the
// provider's TestURL is the probe target and its host becomes the inner
// Host header.
//
// HostAliases on the provider are not expanded — TestURL points at the
// provider's ping endpoint which is already CDN-hosted, so the request
// reaches our backend through the front when the path works.
func CandidatesFromConfig(cfg *domainfront.Config) ([]Candidate, error) {
	if cfg == nil {
		return nil, errors.New("nil config")
	}
	var out []Candidate
	for name, p := range cfg.Providers {
		if p == nil {
			continue
		}
		innerHost, err := innerHostFromTestURL(p.TestURL)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		providerVerify := ""
		if p.VerifyHostname != nil {
			providerVerify = *p.VerifyHostname
		}
		for _, m := range p.Masquerades {
			if m == nil {
				continue
			}
			c := Candidate{
				Provider:  name,
				Domain:    m.Domain,
				IPAddress: m.IpAddress,
				SNI:       m.SNI,
				TestURL:   p.TestURL,
				InnerHost: innerHost,
			}
			if m.VerifyHostname != nil {
				c.VerifyHostname = *m.VerifyHostname
			} else {
				c.VerifyHostname = providerVerify
			}
			out = append(out, c)
		}
	}
	return out, nil
}

func innerHostFromTestURL(testURL string) (string, error) {
	if testURL == "" {
		return "", errors.New("empty TestURL")
	}
	u, err := url.Parse(testURL)
	if err != nil {
		return "", fmt.Errorf("parse TestURL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("TestURL %q has no host", testURL)
	}
	return u.Hostname(), nil
}

// SNIsForProvider returns the distinct, non-empty masquerade domains for
// the named provider in cfg. Used as the outer-SNI pool for
// CloudFrontCandidates (and an equivalent Akamai discovery flow when
// regex-generated hostnames aren't desired).
func SNIsForProvider(cfg *domainfront.Config, provider string) []string {
	if cfg == nil {
		return nil
	}
	p := cfg.Providers[provider]
	if p == nil {
		return nil
	}
	seen := make(map[string]bool, len(p.Masquerades))
	var out []string
	for _, m := range p.Masquerades {
		if m == nil || m.Domain == "" {
			continue
		}
		if seen[m.Domain] {
			continue
		}
		seen[m.Domain] = true
		out = append(out, m.Domain)
	}
	return out
}

// TrustedCAsPool builds an x509.CertPool from a domainfront.Config's
// TrustedCAs. Passes to Options.RootCAs so the probe verifies the front's
// cert chain against the same set domainfront uses in production.
func TrustedCAsPool(cfg *domainfront.Config) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	for _, ca := range cfg.TrustedCAs {
		if ca == nil || ca.Cert == "" {
			continue
		}
		block, _ := pem.Decode([]byte(ca.Cert))
		if block == nil {
			return nil, fmt.Errorf("CA %q: PEM decode failed", ca.CommonName)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("CA %q: parse: %w", ca.CommonName, err)
		}
		pool.AddCert(cert)
	}
	return pool, nil
}
