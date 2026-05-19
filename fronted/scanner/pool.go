package scanner

import (
	"context"
	"errors"
	"fmt"

	"github.com/getlantern/domainfront"
)

// PoolOptions composes a candidate pool from the three feeder sources.
type PoolOptions struct {
	Config *domainfront.Config

	KnownSample      int
	CloudFrontSample int
	AkamaiSample     int

	Resolver Resolver
}

// BuildPool returns a probe pool combining (a) pre-validated masquerades
// from cfg, (b) random CloudFront IP × random masquerade-SNI pairs, and
// (c) Akamai hostnames generated from the MahsaNG/Psiphon regex and
// resolved via opts.Resolver.
//
// Probe target (TestURL, inner Host) for each candidate comes from its
// originating provider's TestURL in cfg.
//
// Sample sizes <= 0 disable the corresponding feeder. When AkamaiSample
// > 0, the canonical AkamaiEdgeHostnames are always included alongside
// the regex-generated draws — it's the highest-trust hostname in the
// pool.
//
// The raw-range feeders (Akamai DNS + CloudFront prefixes) produce
// per-scan-fresh IPs, which match Samim Mirhosseini's "different per
// ISP, location, time of day" observation. The Known feeder (pre-
// resolved IPs from fronted.yaml.gz) is opt-in via KnownSample > 0;
// it's higher-hit-rate when the YAML is current but goes stale faster
// than the raw range scans can self-heal.
func BuildPool(ctx context.Context, opts PoolOptions) ([]Candidate, error) {
	if opts.Config == nil {
		return nil, errors.New("BuildPool: nil Config")
	}

	var cands []Candidate
	if opts.KnownSample > 0 {
		known, err := CandidatesFromConfig(opts.Config)
		if err != nil {
			return nil, fmt.Errorf("known masquerades: %w", err)
		}
		if opts.KnownSample < len(known) {
			known = sampleN(known, opts.KnownSample)
		}
		cands = append(cands, known...)
	}

	akamaiProv := opts.Config.Providers["akamai"]
	if akamaiProv != nil && akamaiProv.TestURL != "" && opts.AkamaiSample > 0 {
		innerHost, err := innerHostFromTestURL(akamaiProv.TestURL)
		if err == nil {
			hostnames := append([]string{}, AkamaiEdgeHostnames...)
			more, err := GenerateAkamaiHostnames(opts.AkamaiSample)
			if err == nil {
				hostnames = append(hostnames, more...)
			}
			akCands, err := AkamaiCandidates(ctx, hostnames, opts.Resolver, akamaiProv.TestURL, innerHost)
			if err == nil {
				cands = append(cands, akCands...)
			}
		}
	}

	cfProv := opts.Config.Providers["cloudfront"]
	if cfProv != nil && cfProv.TestURL != "" && opts.CloudFrontSample > 0 {
		innerHost, err := innerHostFromTestURL(cfProv.TestURL)
		if err == nil {
			snis := SNIsForProvider(opts.Config, "cloudfront")
			if len(snis) > 0 {
				cfCands, err := CloudFrontCandidates(opts.CloudFrontSample, snis, cfProv.TestURL, innerHost)
				if err == nil {
					cands = append(cands, cfCands...)
				}
			}
		}
	}

	return cands, nil
}

func sampleN(cands []Candidate, n int) []Candidate {
	if n >= len(cands) {
		return cands
	}
	out := make([]Candidate, len(cands))
	copy(out, cands)
	for i := 0; i < n; i++ {
		j, _ := pickInt(len(out) - i)
		out[i], out[i+j] = out[i+j], out[i]
	}
	return out[:n]
}
