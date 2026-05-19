package scanner

import (
	"bufio"
	"crypto/rand"
	_ "embed"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strings"
)

//go:embed cloudfront_prefixes.txt
var cloudFrontPrefixesRaw string

// CloudFrontPrefixes returns the embedded CloudFront IPv4 prefix list.
// Edges anywhere in this range route by Host header, so any IP in any
// prefix is a viable outer dial target for an inner Host that points at
// our CloudFront distribution.
func CloudFrontPrefixes() ([]netip.Prefix, error) {
	scanner := bufio.NewScanner(strings.NewReader(cloudFrontPrefixesRaw))
	var out []netip.Prefix
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := netip.ParsePrefix(line)
		if err != nil {
			return nil, fmt.Errorf("parse %q: %w", line, err)
		}
		out = append(out, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no prefixes in embedded list")
	}
	return out, nil
}

// CloudFrontCandidates produces n probe candidates by pairing IPs sampled
// from the embedded CloudFront IP range with outer SNIs randomly drawn
// from snis.
//
// Expect a hit rate below 100%: CloudFront edges serve a subset of
// distributions per POP, so an arbitrary (IP, outer SNI) pair only
// connects when that POP serves both the outer SNI's distribution and
// the inner-Host distribution. The probe filters the survivors.
//
// snis should list CloudFront-fronted hostnames known to be globally
// served (Price Class All) — the masquerade domains in fronted.yaml.gz
// are the natural source.
func CloudFrontCandidates(n int, snis []string, testURL, innerHost string) ([]Candidate, error) {
	if n <= 0 {
		return nil, nil
	}
	if len(snis) == 0 {
		return nil, fmt.Errorf("no outer SNIs supplied")
	}
	prefixes, err := CloudFrontPrefixes()
	if err != nil {
		return nil, err
	}

	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		ip, err := samplePrefix(prefixes)
		if err != nil {
			return out, err
		}
		sniIdx, err := rand.Int(rand.Reader, big.NewInt(int64(len(snis))))
		if err != nil {
			return out, fmt.Errorf("rand: %w", err)
		}
		sni := snis[sniIdx.Int64()]
		out = append(out, Candidate{
			Provider:       "cloudfront",
			Domain:         sni,
			IPAddress:      ip,
			SNI:            sni,
			VerifyHostname: sni,
			TestURL:        testURL,
			InnerHost:      innerHost,
		})
	}
	return out, nil
}

// samplePrefix picks a prefix weighted by its address count, then a
// uniform random IP inside it. Weighting matters because the CloudFront
// list mixes /14s with /27s — uniform-over-prefixes would massively
// over-represent the small ones.
func samplePrefix(prefixes []netip.Prefix) (string, error) {
	if len(prefixes) == 0 {
		return "", fmt.Errorf("no prefixes")
	}

	weights := make([]*big.Int, len(prefixes))
	total := new(big.Int)
	for i, p := range prefixes {
		host := p.Bits()
		bits := p.Addr().BitLen() - host
		w := new(big.Int).Lsh(big.NewInt(1), uint(bits))
		weights[i] = w
		total.Add(total, w)
	}

	pick, err := rand.Int(rand.Reader, total)
	if err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}

	acc := new(big.Int)
	for i, w := range weights {
		acc.Add(acc, w)
		if pick.Cmp(acc) < 0 {
			return randomIPInPrefix(prefixes[i])
		}
	}
	return randomIPInPrefix(prefixes[len(prefixes)-1])
}

func randomIPInPrefix(p netip.Prefix) (string, error) {
	if !p.Addr().Is4() {
		return "", fmt.Errorf("v6 prefix not supported yet: %s", p)
	}
	host := p.Bits()
	bits := 32 - host
	if bits == 0 {
		return p.Addr().String(), nil
	}
	cap := new(big.Int).Lsh(big.NewInt(1), uint(bits))
	pick, err := rand.Int(rand.Reader, cap)
	if err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}

	base := p.Addr().As4()
	baseUint := binary.BigEndian.Uint32(base[:])
	offset := uint32(pick.Int64())
	addrUint := baseUint + offset

	var out [4]byte
	binary.BigEndian.PutUint32(out[:], addrUint)
	return net.IP(out[:]).String(), nil
}
