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
	sbo "github.com/sagernet/sing-box/option"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/fronted/scanner"
)

var errNilConfig = errors.New("meek: ProviderConfig.Config is nil")

// MeekOutboundOptions mirrors lantern-box/option.MeekOutboundOptions
// kept local to avoid version-coupling radiance to lantern-box's release
// cadence. The JSON tags are identical, so once lantern-box ships meek
// and radiance bumps the dep, drop this copy and import the upstream
// type directly.
type MeekOutboundOptions struct {
	sbo.DialerOptions

	URL    string            `json:"url"`
	Fronts []FrontSpec       `json:"fronts"`
	Header map[string]string `json:"header,omitempty"`

	PollIntervalMs int    `json:"poll_interval_ms,omitempty"`
	MaxBodyBytes   int    `json:"max_body_bytes,omitempty"`
	SessionIDLen   int    `json:"session_id_len,omitempty"`
	ConnectTimeout string `json:"connect_timeout,omitempty"`
	ReadTimeout    string `json:"read_timeout,omitempty"`
}

// MeekOutboundType is the sing-box outbound type string. Matches
// lantern-box/constant.TypeMeek. Stringified so radiance doesn't have
// to import a version of lantern-box that registers meek.
const MeekOutboundType = "meek"

// DefaultURL is the inner Host header sent through the meek tunnel.
// It is never resolved or dialed; callers supply the real SNI and dial
// target via FrontSpec.
const DefaultURL = "https://meek.dsa.akamai.getiantem.org/"

// BuildOutbound returns a sing-box outbound for the meek transport with
// the given tag, meek-server URL, and front pool. The returned Outbound
// can be appended directly to O.Options.Outbounds; selector groups can
// reference it by tag.
//
// Returns ok=false when fronts is empty — without at least one front,
// the meek outbound has nothing to dial and would fail at first use.
// Callers should skip injection in that case.
func BuildOutbound(tag, url string, fronts []FrontSpec) (sbo.Outbound, bool) {
	if len(fronts) == 0 {
		return sbo.Outbound{}, false
	}
	return sbo.Outbound{
		Type: MeekOutboundType,
		Tag:  tag,
		Options: &MeekOutboundOptions{
			URL:    url,
			Fronts: fronts,
		},
	}, true
}

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
//
// Discovery is raw-range-primary by default: fresh IPs from the AWS
// CloudFront prefix list and DNS-resolved Akamai edges produce
// per-(ISP, location, time-of-day) candidates rather than the same
// baked list every user sees. fronted.yaml.gz is consulted only for
// outer-SNI pools, trusted CAs, and host-alias mappings (not its
// pre-resolved IPs) unless KnownSample > 0.
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
	if c.CloudFrontSample == 0 {
		c.CloudFrontSample = 30
	}
	if c.AkamaiSample == 0 {
		c.AkamaiSample = 3
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
