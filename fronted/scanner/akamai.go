package scanner

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net"
)

// Resolver resolves a hostname to one or more IPv4 addresses.
// Implementations must not route DNS through the VPN tunnel — the OS /
// ISP resolver is the right path in IR because the ISP returns real
// Akamai IPs reachable from its own network and DoH/DoT endpoints are
// themselves blocked.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// SystemResolver wraps the OS resolver. Use it for Akamai edge hostnames
// (a248.e.akamai.net and similar) which Iran's ISP resolvers return
// truthfully because Akamai hosts too much Iranian critical
// infrastructure to be blanket-blocked.
//
// Never use this for our own backend hostnames — those get poisoned.
type SystemResolver struct{}

func (SystemResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	r := &net.Resolver{}
	addrs, err := r.LookupHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("lookup %s: %w", host, err)
	}
	v4 := addrs[:0]
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil {
			continue
		}
		if v4ip := ip.To4(); v4ip != nil {
			v4 = append(v4, v4ip.String())
		}
	}
	if len(v4) == 0 {
		return nil, fmt.Errorf("lookup %s: no IPv4", host)
	}
	return v4, nil
}

// AkamaiEdgeHostnames is the canonical Akamai edge hostname used by every
// masquerade in our existing fronted.yaml.gz Akamai provider. The IPs
// returned by the OS resolver for this hostname are geographically
// relevant to the client's network — exactly the per-(ISP, location)
// signal we want. Additional hostnames from the MahsaNG regex pattern
// can be appended to widen the candidate space.
var AkamaiEdgeHostnames = []string{
	"a248.e.akamai.net",
}

// GenerateAkamaiHostnames produces n random hostnames matching the regex
// `a([1-9]|1[0-9])([0-9]{2})\.(dsc)?(b|d|g|g2|na|r|w7)\.akamai\.net`,
// matching the pattern Psiphon and MahsaNG use. The regex enumerates
// roughly 3,500 distinct hostnames; each is a valid Akamai edge that
// the OS resolver answers from the general edge pool. Fresh hostname per
// dial varies the outer SNI without changing which property is reached.
func GenerateAkamaiHostnames(n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		h, err := randomAkamaiHostname()
		if err != nil {
			return out, err
		}
		out = append(out, h)
	}
	return out, nil
}

func randomAkamaiHostname() (string, error) {
	firstPart, err := pickInt(19)
	if err != nil {
		return "", err
	}
	first := firstPart + 1

	rest, err := pickInt(100)
	if err != nil {
		return "", err
	}

	dscFlip, err := pickInt(2)
	if err != nil {
		return "", err
	}
	dsc := ""
	if dscFlip == 1 {
		dsc = "dsc"
	}

	suffixes := []string{"b", "d", "g", "g2", "na", "r", "w7"}
	suf, err := pickInt(len(suffixes))
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("a%d%02d.%s%s.akamai.net", first, rest, dsc, suffixes[suf]), nil
}

func pickInt(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("rand: %w", err)
	}
	return int(v.Int64()), nil
}

// AkamaiCertHostname is the hostname every Akamai edge's default cert
// validates as (alongside *.akamaized.net, *.akamaihd.net, etc.). Used
// for post-handshake cert verification regardless of which hostname we
// looked up to discover the edge IP — the regex-generated hostnames
// (a1798.dscg.akamai.net, etc.) are useful for DNS-side discovery but
// aren't in the served cert's SANs.
const AkamaiCertHostname = "a248.e.akamai.net"

// AkamaiCandidates resolves the supplied hostnames via resolver and
// produces one Candidate per distinct resolved IPv4. SNI is left empty
// (matches production: Akamai edges serve their default cert when SNI
// is omitted). VerifyHostname is AkamaiCertHostname for every entry.
//
// hostnames may be the canonical AkamaiEdgeHostnames (1 hostname,
// stable IPs from the resolver), the MahsaNG-style regex hostnames
// (varied hostnames, more IP diversity), or both mixed.
func AkamaiCandidates(ctx context.Context, hostnames []string, resolver Resolver, testURL, innerHost string) ([]Candidate, error) {
	if resolver == nil {
		resolver = SystemResolver{}
	}
	if len(hostnames) == 0 {
		hostnames = AkamaiEdgeHostnames
	}

	var out []Candidate
	var firstErr error
	seenIP := make(map[string]bool)
	for _, h := range hostnames {
		ips, err := resolver.LookupHost(ctx, h)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, ip := range ips {
			if seenIP[ip] {
				continue
			}
			seenIP[ip] = true
			out = append(out, Candidate{
				Provider:       "akamai",
				Domain:         h,
				IPAddress:      ip,
				VerifyHostname: AkamaiCertHostname,
				TestURL:        testURL,
				InnerHost:      innerHost,
			})
		}
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}
