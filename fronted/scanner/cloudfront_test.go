package scanner

import (
	"net"
	"net/netip"
	"testing"
)

func TestCloudFrontPrefixes_NonEmpty(t *testing.T) {
	p, err := CloudFrontPrefixes()
	if err != nil {
		t.Fatalf("CloudFrontPrefixes: %v", err)
	}
	if len(p) < 100 {
		t.Errorf("got %d prefixes; want >= 100 (AWS publishes ~200)", len(p))
	}
}

func TestSamplePrefix_HitsPrefix(t *testing.T) {
	prefixes := []netip.Prefix{
		netip.MustParsePrefix("203.0.113.0/24"),
	}
	for i := 0; i < 50; i++ {
		ip, err := samplePrefix(prefixes)
		if err != nil {
			t.Fatalf("samplePrefix: %v", err)
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			t.Fatalf("parse %q: %v", ip, err)
		}
		if !prefixes[0].Contains(addr) {
			t.Errorf("sampled %s not in %s", ip, prefixes[0])
		}
	}
}

func TestSamplePrefix_WeightedByCount(t *testing.T) {
	prefixes := []netip.Prefix{
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/30"),
	}
	bigHits, smallHits := 0, 0
	for i := 0; i < 1000; i++ {
		ip, err := samplePrefix(prefixes)
		if err != nil {
			t.Fatalf("samplePrefix: %v", err)
		}
		addr := netip.MustParseAddr(ip)
		if prefixes[0].Contains(addr) {
			bigHits++
		} else if prefixes[1].Contains(addr) {
			smallHits++
		}
	}
	if bigHits <= smallHits {
		t.Errorf("/24 hit %d, /30 hit %d — expected /24 dominant", bigHits, smallHits)
	}
}

func TestCloudFrontCandidates(t *testing.T) {
	snis := []string{"aa1.awsstatic.com", "advertising.amazon.com", "abcmouse.com"}
	cands, err := CloudFrontCandidates(30, snis, "https://api.iantem.io/ping", "api.iantem.io")
	if err != nil {
		t.Fatalf("CloudFrontCandidates: %v", err)
	}
	if len(cands) != 30 {
		t.Errorf("len = %d; want 30", len(cands))
	}

	seenIP := map[string]int{}
	sniHits := map[string]int{}
	allowedSNI := map[string]bool{"aa1.awsstatic.com": true, "advertising.amazon.com": true, "abcmouse.com": true}
	for _, c := range cands {
		if c.Provider != "cloudfront" {
			t.Errorf("provider = %q; want cloudfront", c.Provider)
		}
		if !allowedSNI[c.Domain] {
			t.Errorf("Domain = %q; not in input SNI list", c.Domain)
		}
		if c.Domain != c.VerifyHostname {
			t.Errorf("VerifyHostname = %q; want = Domain %q", c.VerifyHostname, c.Domain)
		}
		if net.ParseIP(c.IPAddress) == nil {
			t.Errorf("bad IP %q", c.IPAddress)
		}
		if c.InnerHost != "api.iantem.io" {
			t.Errorf("InnerHost = %q; want api.iantem.io", c.InnerHost)
		}
		seenIP[c.IPAddress]++
		sniHits[c.Domain]++
	}
	if len(seenIP) < 25 {
		t.Errorf("only %d distinct IPs across 30 samples; want more variety", len(seenIP))
	}
	if len(sniHits) < 2 {
		t.Errorf("SNIs got %d unique hits; want at least 2 (random distribution)", len(sniHits))
	}
}

func TestCloudFrontCandidates_NoSNIs(t *testing.T) {
	_, err := CloudFrontCandidates(5, nil, "https://api.iantem.io/ping", "api.iantem.io")
	if err == nil {
		t.Errorf("expected error when snis is empty")
	}
}
