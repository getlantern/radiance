// Package meek wires radiance's fronted/scanner output into the
// sing-box meek outbound config shape. A Provider holds a scanner
// Service, samples its current working list, and produces FrontSpec
// JSON entries suitable for inclusion in a sing-box meek outbound
// configuration.
package meek

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/getlantern/domainfront"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/fronted/scanner"
)

var errNilConfig = errors.New("meek: ProviderConfig.Config is nil")

// FrontSpec mirrors lantern-box/option.FrontSpec; kept local to avoid
// version-coupling radiance's release cadence to lantern-box's.
type FrontSpec struct {
	IPAddress      string `json:"ip_address"`
	SNI            string `json:"sni,omitempty"`
	VerifyHostname string `json:"verify_hostname,omitempty"`
}

// ProviderConfig configures the bridge between scanner and meek
// outbound. Defaults are tuned for IR usage: a 1h refresh interval is
// short enough to react to CDN block churn, a 6h cache TTL means a
// reboot loads the recent working list rather than re-scanning cold.
type ProviderConfig struct {
	Config    *domainfront.Config
	CacheFile string

	RefreshInterval  time.Duration
	CacheTTL         time.Duration
	KnownSample      int
	CloudFrontSample int
	AkamaiSample     int

	Logger *slog.Logger
}

func (c *ProviderConfig) defaults() {
	if c.KnownSample == 0 {
		c.KnownSample = 50
	}
	if c.CloudFrontSample == 0 {
		c.CloudFrontSample = 10
	}
	if c.AkamaiSample == 0 {
		c.AkamaiSample = 5
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Provider runs a scanner Service over the supplied domainfront config
// and exposes the working-front list as []FrontSpec for the meek
// outbound.
type Provider struct {
	service *scanner.Service
}

// NewProvider constructs a Provider. The scanner is configured to dial
// through radiance/bypass so its probes don't loop through the active
// VPN TUN; cert validation uses the trusted-CA pool from cfg. Call
// Start to begin background scanning.
func NewProvider(cfg ProviderConfig) (*Provider, error) {
	if cfg.Config == nil {
		return nil, errNilConfig
	}
	cfg.defaults()

	rootCAs, err := scanner.TrustedCAsPool(cfg.Config)
	if err != nil {
		return nil, err
	}

	svc, err := scanner.NewService(scanner.ServiceConfig{
		Config:           cfg.Config,
		CacheFile:        cfg.CacheFile,
		RefreshInterval:  cfg.RefreshInterval,
		CacheTTL:         cfg.CacheTTL,
		KnownSample:      cfg.KnownSample,
		CloudFrontSample: cfg.CloudFrontSample,
		AkamaiSample:     cfg.AkamaiSample,
		Probe: scanner.ProbeOptions{
			Dialer:  bypassDialer{},
			RootCAs: rootCAs,
		},
		Logger: cfg.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &Provider{service: svc}, nil
}

// Start kicks off the background refresh loop and returns immediately.
// The loop runs until ctx is canceled or Close is called.
func (p *Provider) Start(ctx context.Context) {
	go p.service.Start(ctx)
}

// Close stops the background loop. Idempotent.
func (p *Provider) Close() error { return p.service.Close() }

// FrontSpecs returns up to n working fronts in the meek-outbound JSON
// shape. n <= 0 returns all. The list is ordered by ascending latency.
func (p *Provider) FrontSpecs(n int) []FrontSpec {
	return resultsToFrontSpecs(p.service.Working(), n)
}

func resultsToFrontSpecs(working []scanner.Result, n int) []FrontSpec {
	if n > 0 && n < len(working) {
		working = working[:n]
	}
	out := make([]FrontSpec, 0, len(working))
	for _, r := range working {
		out = append(out, FrontSpec{
			IPAddress:      r.Candidate.IPAddress,
			SNI:            r.Candidate.SNI,
			VerifyHostname: r.Candidate.VerifyHostname,
		})
	}
	return out
}

// ReportFailure passes a meek dial failure back to the scanner so the
// underlying front gets dropped after enough failures and the next
// refresh runs sooner. spec.IPAddress is the load-bearing key.
func (p *Provider) ReportFailure(spec FrontSpec) {
	p.service.ReportFailure(scanner.Candidate{
		IPAddress: spec.IPAddress,
		SNI:       spec.SNI,
	})
}

type bypassDialer struct{}

func (bypassDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return bypass.DialContext(ctx, network, addr)
}
