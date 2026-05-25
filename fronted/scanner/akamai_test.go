package scanner

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

type fakeResolver struct {
	answers map[string][]string
	err     map[string]error
}

func (f fakeResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	if err, ok := f.err[host]; ok {
		return nil, err
	}
	if a, ok := f.answers[host]; ok {
		return a, nil
	}
	return nil, errors.New("no answer")
}

func TestAkamaiCandidates_Dedup(t *testing.T) {
	r := fakeResolver{answers: map[string][]string{
		"a248.e.akamai.net": {"23.47.48.1", "23.47.48.2", "23.47.48.1"},
	}}
	cands, err := AkamaiCandidates(context.Background(), nil, nil, r, "https://api.iantem.io/ping", "api.iantem.io")
	if err != nil {
		t.Fatalf("AkamaiCandidates: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("len = %d; want 2 (dedup)", len(cands))
	}
	for _, c := range cands {
		if c.Provider != "akamai" {
			t.Errorf("provider = %q", c.Provider)
		}
		if c.Domain != "a248.e.akamai.net" {
			t.Errorf("Domain = %q", c.Domain)
		}
	}
}

func TestAkamaiCandidates_MixesNamedSNIs(t *testing.T) {
	r := fakeResolver{answers: map[string][]string{
		"a248.e.akamai.net": {"23.47.48.1", "23.47.48.2"},
	}}
	snis := []string{"python.org", "pypi.org", "snapp.ir", "google.com", "aparat.com"}
	cands, err := AkamaiCandidates(context.Background(), nil, snis, r, "https://api.iantem.io/ping", "api.iantem.io")
	if err != nil {
		t.Fatalf("AkamaiCandidates: %v", err)
	}
	if want := 2 * (1 + akamaiSNIsPerIP); len(cands) != want {
		t.Errorf("len = %d; want %d", len(cands), want)
	}

	byIP := map[string][]Candidate{}
	for _, c := range cands {
		byIP[c.IPAddress] = append(byIP[c.IPAddress], c)
		if c.VerifyHostname != AkamaiCertHostname {
			t.Errorf("VerifyHostname = %q; want %s", c.VerifyHostname, AkamaiCertHostname)
		}
	}
	for ip, group := range byIP {
		if group[0].SNI != "" {
			t.Errorf("IP %s: first candidate SNI = %q; want empty", ip, group[0].SNI)
		}
		seen := map[string]bool{}
		for _, c := range group[1:] {
			if c.SNI == "" {
				t.Errorf("IP %s: named candidate has empty SNI", ip)
			}
			if seen[c.SNI] {
				t.Errorf("IP %s: SNI %q appears twice — should be without replacement", ip, c.SNI)
			}
			seen[c.SNI] = true
		}
	}
}

func TestAkamaiCandidates_MultipleHostnames(t *testing.T) {
	r := fakeResolver{answers: map[string][]string{
		"a248.e.akamai.net": {"23.47.48.1"},
		"a123.b.akamai.net": {"184.150.1.1"},
	}}
	hostnames := []string{"a248.e.akamai.net", "a123.b.akamai.net"}
	cands, err := AkamaiCandidates(context.Background(), hostnames, nil, r, "https://api.iantem.io/ping", "api.iantem.io")
	if err != nil {
		t.Fatalf("AkamaiCandidates: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("len = %d; want 2", len(cands))
	}
	domains := map[string]bool{}
	for _, c := range cands {
		domains[c.Domain] = true
	}
	if len(domains) != 2 {
		t.Errorf("expected both hostnames, got %v", domains)
	}
}

func TestAkamaiCandidates_AllResolversFail(t *testing.T) {
	r := fakeResolver{err: map[string]error{
		"a248.e.akamai.net": errors.New("dns blocked"),
	}}
	_, err := AkamaiCandidates(context.Background(), nil, nil, r, "https://api.iantem.io/ping", "api.iantem.io")
	if err == nil {
		t.Errorf("expected error when all lookups fail")
	}
}

func TestGenerateAkamaiHostnames_MatchesRegex(t *testing.T) {
	pattern := regexp.MustCompile(`^a([1-9]|1[0-9])([0-9]{2})\.(dsc)?(b|d|g|g2|na|r|w7)\.akamai\.net$`)
	hostnames, err := GenerateAkamaiHostnames(200)
	if err != nil {
		t.Fatalf("GenerateAkamaiHostnames: %v", err)
	}
	if len(hostnames) != 200 {
		t.Errorf("len = %d; want 200", len(hostnames))
	}
	for _, h := range hostnames {
		if !pattern.MatchString(h) {
			t.Errorf("hostname %q doesn't match Akamai edge pattern", h)
		}
	}
}

func TestGenerateAkamaiHostnames_Variety(t *testing.T) {
	hostnames, err := GenerateAkamaiHostnames(200)
	if err != nil {
		t.Fatalf("GenerateAkamaiHostnames: %v", err)
	}
	unique := map[string]bool{}
	for _, h := range hostnames {
		unique[h] = true
	}
	// 200 draws from ~3,500-name space, birthday paradox aside, should see >100 distinct.
	if len(unique) < 100 {
		t.Errorf("only %d distinct hostnames across 200 draws; want > 100", len(unique))
	}
}

func TestAkamaiCandidates_PartialFailureStillReturns(t *testing.T) {
	r := fakeResolver{
		answers: map[string][]string{"a248.e.akamai.net": {"23.47.48.1"}},
		err:     map[string]error{"a999.z.akamai.net": errors.New("nxdomain")},
	}
	hostnames := []string{"a248.e.akamai.net", "a999.z.akamai.net"}
	cands, err := AkamaiCandidates(context.Background(), hostnames, nil, r, "https://api.iantem.io/ping", "api.iantem.io")
	if err != nil {
		t.Fatalf("expected nil err when at least one lookup succeeded, got %v", err)
	}
	if len(cands) != 1 {
		t.Errorf("len = %d; want 1", len(cands))
	}
}
