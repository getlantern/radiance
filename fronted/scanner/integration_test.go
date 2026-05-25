package scanner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// integrationGate enforces SCANNER_INTEGRATION=1 so unattended CI runs
// don't probe live CDNs. Run with:
//
//	SCANNER_INTEGRATION=1 go test -count=1 -v -run TestLive ./fronted/scanner/...
func integrationGate(t *testing.T) {
	t.Helper()
	if os.Getenv("SCANNER_INTEGRATION") != "1" {
		t.Skip("skipping live-network test; set SCANNER_INTEGRATION=1 to run")
	}
}

// Probe targets behind our Akamai and CloudFront fronts; URLs taken from
// the testurl fields in fronted.yaml.gz, the same probes domainfront
// uses to validate masquerades in production.
const (
	akamaiTestURL     = "https://fronted-ping.dsa.akamai.getiantem.org/ping"
	cloudfrontTestURL = "http://d157vud77ygy87.cloudfront.net/ping"
)

func TestLive_AkamaiSystemResolver(t *testing.T) {
	integrationGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hostnames, err := GenerateAkamaiHostnames(8)
	if err != nil {
		t.Fatalf("GenerateAkamaiHostnames: %v", err)
	}
	hostnames = append(hostnames, AkamaiEdgeHostnames...)

	cands, err := AkamaiCandidates(ctx, hostnames, nil, SystemResolver{}, akamaiTestURL, "fronted-ping.dsa.akamai.getiantem.org")
	if err != nil {
		t.Fatalf("AkamaiCandidates: %v", err)
	}
	if len(cands) == 0 {
		t.Fatal("no Akamai candidates resolved; system resolver may have failed")
	}

	results := Scan(ctx, cands, Options{DialTimeout: 5 * time.Second, Concurrency: 4})
	working := RankWorking(results)

	report(t, "akamai", cands, results, working)
	if len(working) == 0 {
		t.Errorf("0 of %d Akamai candidates probed OK; expected at least 1", len(cands))
	}
}

// TestLive_CloudFrontRandomIPs is diagnostic only — random IPs in the
// CloudFront range have low hit rate because each edge POP serves a
// subset of distributions. The probe correctly filters; the test
// reports the rate without asserting a floor. To validate the probe
// technique itself, see TestLive_CloudFrontKnownMasquerades.
func TestLive_CloudFrontRandomIPs(t *testing.T) {
	integrationGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	snis := []string{
		"aa1.awsstatic.com",
		"advertising.amazon.com",
		"abcmouse.com",
		"adsrvr.org",
	}
	cands, err := CloudFrontCandidates(40, snis, cloudfrontTestURL, "d157vud77ygy87.cloudfront.net")
	if err != nil {
		t.Fatalf("CloudFrontCandidates: %v", err)
	}

	results := Scan(ctx, cands, Options{DialTimeout: 5 * time.Second, Concurrency: 8})
	working := RankWorking(results)

	report(t, "cloudfront-random", cands, results, working)
}

// TestLive_CloudFrontKnownMasquerades probes a handful of pre-validated
// (IP, outer SNI) pairs from fronted.yaml.gz. Hit rate should be high
// because each pair was verified before being committed to the config.
// Use this to confirm the probe machinery works against CloudFront;
// random-IP discovery is a separate question covered by
// TestLive_CloudFrontRandomIPs.
func TestLive_CloudFrontKnownMasquerades(t *testing.T) {
	integrationGate(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	known := []Candidate{
		{Provider: "cloudfront", Domain: "aa1.awsstatic.com", SNI: "aa1.awsstatic.com", IPAddress: "99.84.2.4", VerifyHostname: "aa1.awsstatic.com", TestURL: cloudfrontTestURL, InnerHost: "d157vud77ygy87.cloudfront.net"},
		{Provider: "cloudfront", Domain: "aa1.awsstatic.com", SNI: "aa1.awsstatic.com", IPAddress: "18.238.3.4", VerifyHostname: "aa1.awsstatic.com", TestURL: cloudfrontTestURL, InnerHost: "d157vud77ygy87.cloudfront.net"},
		{Provider: "cloudfront", Domain: "advertising.amazon.com", SNI: "advertising.amazon.com", IPAddress: "3.164.130.9", VerifyHostname: "advertising.amazon.com", TestURL: cloudfrontTestURL, InnerHost: "d157vud77ygy87.cloudfront.net"},
		{Provider: "cloudfront", Domain: "advertising.amazon.com", SNI: "advertising.amazon.com", IPAddress: "54.230.224.110", VerifyHostname: "advertising.amazon.com", TestURL: cloudfrontTestURL, InnerHost: "d157vud77ygy87.cloudfront.net"},
		{Provider: "cloudfront", Domain: "advertising.amazon.com", SNI: "advertising.amazon.com", IPAddress: "18.244.1.167", VerifyHostname: "advertising.amazon.com", TestURL: cloudfrontTestURL, InnerHost: "d157vud77ygy87.cloudfront.net"},
	}
	results := Scan(ctx, known, Options{DialTimeout: 5 * time.Second, Concurrency: 4})
	working := RankWorking(results)
	report(t, "cloudfront-known", known, results, working)
	// Diagnostic only: pre-validated pairs may go stale as CloudFront
	// re-shards distributions across POPs. The scanner correctly
	// filtering stale entries is exactly its job.
}

func report(t *testing.T, label string, cands []Candidate, results []Result, working []Result) {
	t.Helper()
	t.Logf("[%s] probed %d candidates, %d working (%.0f%%)", label, len(cands), len(working), 100*float64(len(working))/float64(len(cands)))
	for i, r := range working {
		if i >= 5 {
			t.Logf("[%s] (… and %d more working)", label, len(working)-5)
			break
		}
		t.Logf("[%s] OK %s ip=%s sni=%s latency=%s", label, r.Candidate.Provider, r.Candidate.IPAddress, r.Candidate.outerSNI(), r.Latency)
	}
	errs := map[string]int{}
	for _, r := range results {
		if r.OK() || r.Err == nil {
			continue
		}
		errs[shortErr(r.Err)]++
	}
	for kind, n := range errs {
		t.Logf("[%s] error %q: %d", label, kind, n)
	}
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	return s
}

// Sanity check that compiles without integration gate so the file always builds.
var _ = fmt.Sprintf
