package meek

import (
	"testing"
	"time"

	"github.com/getlantern/domainfront"
	"github.com/getlantern/radiance/fronted/scanner"
)

func TestNewProvider_NilConfigErrors(t *testing.T) {
	_, err := NewProvider(ProviderConfig{})
	if err == nil {
		t.Errorf("expected error for nil Config")
	}
}

func TestNewProvider_OK(t *testing.T) {
	p, err := NewProvider(ProviderConfig{Config: &domainfront.Config{}})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if got := p.FrontSpecs(5); got == nil {
		t.Errorf("FrontSpecs returned nil; want empty slice")
	}
}

func TestResultsToFrontSpecs_PreservesOrderAndShape(t *testing.T) {
	working := []scanner.Result{
		{Candidate: scanner.Candidate{Provider: "akamai", IPAddress: "23.47.48.230", VerifyHostname: "a248.e.akamai.net"}, Latency: 50 * time.Millisecond, Status: 200},
		{Candidate: scanner.Candidate{Provider: "cloudfront", IPAddress: "99.84.2.4", SNI: "aa1.awsstatic.com", VerifyHostname: "aa1.awsstatic.com"}, Latency: 110 * time.Millisecond, Status: 200},
	}
	got := resultsToFrontSpecs(working, 0)
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	if got[0].IPAddress != "23.47.48.230" || got[0].SNI != "" || got[0].VerifyHostname != "a248.e.akamai.net" {
		t.Errorf("akamai mapping wrong: %+v", got[0])
	}
	if got[1].IPAddress != "99.84.2.4" || got[1].SNI != "aa1.awsstatic.com" {
		t.Errorf("cloudfront mapping wrong: %+v", got[1])
	}
}

func TestResultsToFrontSpecs_LimitsToN(t *testing.T) {
	working := []scanner.Result{
		{Candidate: scanner.Candidate{IPAddress: "1.1.1.1"}, Status: 200},
		{Candidate: scanner.Candidate{IPAddress: "2.2.2.2"}, Status: 200},
		{Candidate: scanner.Candidate{IPAddress: "3.3.3.3"}, Status: 200},
	}
	if got := resultsToFrontSpecs(working, 0); len(got) != 3 {
		t.Errorf("n=0 should return all; got %d", len(got))
	}
	if got := resultsToFrontSpecs(working, 2); len(got) != 2 {
		t.Errorf("n=2 should return 2; got %d", len(got))
	}
	if got := resultsToFrontSpecs(working, 10); len(got) != 3 {
		t.Errorf("n>len should return all; got %d", len(got))
	}
}
