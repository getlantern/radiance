package dnstt

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	_ "embed"

	"github.com/alitto/pond"
	"github.com/getlantern/dnstt"
	"github.com/getlantern/keepcurrent"
	"github.com/goccy/go-yaml"
	"go.opentelemetry.io/otel"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/fileperm"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling/smart"
	"github.com/getlantern/radiance/traces"
)

// DNSTT is an alias for the upstream transport interface, re-exported so
// callers can type variables without importing github.com/getlantern/dnstt directly.
type DNSTT = dnstt.DNSTT

type dnsttConfig struct {
	Domain           string  `yaml:"domain"`    // DNS tunnel domain, e.g., "t.iantem.io"
	PublicKey        string  `yaml:"publicKey"` // DNSTT server public key
	DoHResolver      *string `yaml:"dohResolver,omitempty"`
	DoTResolver      *string `yaml:"dotResolver,omitempty"`
	UTLSDistribution *string `yaml:"utlsDistribution,omitempty"`
}

//go:embed dnstt.yml.gz
var embeddedConfig []byte

var localConfigMutex sync.Mutex

const dnsttConfigURL = "https://raw.githubusercontent.com/getlantern/radiance/main/kindling/dnstt/dnstt.yml.gz"
const pollInterval = 12 * time.Hour
const tracerName = "github.com/getlantern/radiance/kindling/dnstt"

// DNSTTOptions load the embedded DNSTT config and return kindling options so
// it can be used as one of the transport options. If the local config filepath
// is provided and exists, this config will be loaded and if successfully
// parsed, will be returned instead of the embedded config.
func DNSTTOptions(ctx context.Context, localConfigFilepath string, logger io.Writer) (dnstt.DNSTT, error) {
	ctx, span := otel.Tracer(tracerName).Start(
		ctx,
		"DNSTTOptions",
	)
	defer span.End()
	// parsing embedded configs and loading options
	options, err := parseDNSTTConfigs(embeddedConfig)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("failed to parse dnstt embedded config: %w", err))
	}

	// if local config is set and exists, parse, load the dnstt config and close the embedded dns tunnels
	if localConfigFilepath != "" {
		localConfigMutex.Lock()
		defer localConfigMutex.Unlock()
		if config, err := os.ReadFile(localConfigFilepath); err == nil {
			opts, err := parseDNSTTConfigs(config)
			if err != nil {
				span.RecordError(err)
				slog.Warn("failed to parse local dnstt config, returning embedded dnstt config", slog.Any("error", err))
			} else {
				slog.Debug("replacing embedded config by local dnstt config", slog.Int("options", len(opts)))
				options = opts
			}
		} else {
			span.RecordError(err)
			slog.Warn("failed to read local dnstt config file", slog.Any("error", err), slog.String("filepath", localConfigFilepath))
		}
	}
	m := &multipleDNSTTTransport{
		tunChan:  make(chan *dnsTunnel, maxWorkingTunnels),
		stopChan: make(chan struct{}),
		probeCh:  make(chan struct{}, 1),
		configs:  options,
	}
	m.crawlOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("PANIC while searching for working dns tunnels", slog.Any("recover", r), slog.String("stack", string(debug.Stack())))
				}
			}()
			m.findWorkingDNSTunnels()
		}()
	})

	// starting config updater/fetcher
	client, err := smart.NewHTTPClientWithSmartTransport(logger, dnsttConfigURL)
	if err != nil {
		span.RecordError(err)
		slog.Error("couldn't create http client for fetching dnstt configs", slog.Any("error", err))
	}

	dnsttConfigUpdate(ctx, localConfigFilepath, client)
	return m, nil
}

func processYaml(gzippedYaml []byte) ([]dnsttConfig, error) {
	r, gzipErr := gzip.NewReader(bytes.NewReader(gzippedYaml))
	if gzipErr != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %w", gzipErr)
	}
	defer r.Close()
	yml, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read gzipped file: %w", err)
	}
	path, err := yaml.PathString("$.dnsttConfigs")
	if err != nil {
		return nil, fmt.Errorf("failed to create config path: %w", err)
	}
	var cfg []dnsttConfig
	if err = path.Read(bytes.NewReader(yml), &cfg); err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	return cfg, nil
}

func dnsttConfigValidator() func([]byte) error {
	return func(data []byte) error {
		if _, err := processYaml(data); err != nil {
			slog.Error("failed to validate dnstt configuration", "error", err)
			return err
		}
		return nil
	}
}

func dnsttConfigUpdate(ctx context.Context, localConfigPath string, httpClient *http.Client) {
	if httpClient == nil || localConfigPath == "" {
		slog.Warn("missing http client or local config path parameters, required for updating dnstt configuration")
		return
	}
	slog.Debug("Updating dnstt configuration", slog.String("url", dnsttConfigURL))
	source := keepcurrent.FromWebWithClient(dnsttConfigURL, httpClient)
	chDB := make(chan []byte)
	dest := keepcurrent.ToChannel(chDB)
	runner := keepcurrent.NewWithValidator(
		dnsttConfigValidator(),
		source,
		dest,
	)
	stopRunner := runner.Start(pollInterval)
	go func() {
		for {
			select {
			case <-ctx.Done():
				stopRunner()
				return
			case data, ok := <-chDB:
				if !ok {
					return
				}
				slog.Debug("received new dnstt configuration")
				if err := onNewDNSTTConfig(localConfigPath, data); err != nil {
					slog.Error("failed to handle new dnstt configuration", "error", err)
				}
			}
		}
	}()
}

type DNSTTUpdateEvent struct {
	events.Event
	YML string
}

func onNewDNSTTConfig(configFilepath string, gzippedYML []byte) error {
	slog.Debug("received new dnstt configs")
	events.Emit(DNSTTUpdateEvent{
		YML: string(gzippedYML),
	})

	localConfigMutex.Lock()
	defer localConfigMutex.Unlock()
	return atomicfile.WriteFile(configFilepath, gzippedYML, fileperm.File)
}

func cfgResolver(cfg dnsttConfig) string {
	if cfg.DoHResolver != nil {
		return *cfg.DoHResolver
	}
	if cfg.DoTResolver != nil {
		return *cfg.DoTResolver
	}
	return ""
}

func newDNSTT(cfg dnsttConfig) (dnstt.DNSTT, error) {
	opts := make([]dnstt.Option, 0)
	if cfg.Domain != "" {
		opts = append(opts, dnstt.WithTunnelDomain(cfg.Domain))
	}
	if cfg.PublicKey != "" {
		opts = append(opts, dnstt.WithPublicKey(cfg.PublicKey))
	}
	if cfg.DoHResolver != nil {
		opts = append(opts, dnstt.WithDoH(*cfg.DoHResolver))
	}
	if cfg.DoTResolver != nil {
		opts = append(opts, dnstt.WithDoT(*cfg.DoTResolver))
	}
	if cfg.UTLSDistribution != nil {
		opts = append(opts, dnstt.WithUTLSDistribution(*cfg.UTLSDistribution))
	}

	// DNSTT is the last-resort transport, expected to work when the VPN is up
	// but every other transport is blocked. Dialing through bypass keeps its
	// DoH/DoT connections off the TUN so they don't loop back through the tunnel.
	opts = append(opts, dnstt.WithDialer(bypass.DialContext))

	d, err := dnstt.NewDNSTT(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to build new dnstt: %w", err)
	}
	return d, nil
}

func parseDNSTTConfigs(gzipyml []byte) ([]dnsttConfig, error) {
	cfgs, err := processYaml(gzipyml)
	if err != nil {
		return nil, err
	}

	for _, cfg := range cfgs {
		if cfg.Domain == "" || cfg.PublicKey == "" {
			return nil, fmt.Errorf("missing required parameters")
		}
		if cfg.DoHResolver == nil && cfg.DoTResolver == nil {
			return nil, fmt.Errorf("missing resolver")
		}
		if cfg.DoHResolver != nil && cfg.DoTResolver != nil {
			return nil, fmt.Errorf("only one DoTResolver or DoHResolver must be defined, not both")
		}
	}

	return cfgs, nil
}

var waitFor = 5 * time.Minute

func (m *multipleDNSTTTransport) findWorkingDNSTunnels() {
	go m.tryAllDNSTunnels()
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopChan:
			slog.Debug("stopping parallel dialing dns tunnels")
			return
		case <-ticker.C:
			m.tryAllDNSTunnels()
		case <-m.probeCh:
			m.tryAllDNSTunnels()
		}
	}
}

// tryAllDNSTunnels probes configs until tunChan holds maxWorkingTunnels
// established tunnels, then stops. Each probe sends a plain HTTP request
// (not HTTPS) to a known endpoint: this both verifies the tunnel works and
// eagerly establishes a DNSTT session that later requests reuse. HTTPS is
// avoided because the TLS handshake routinely times out over the slow,
// small-MTU tunnel, which would reject otherwise-working tunnels.
//
// It is single-flighted: concurrent triggers (timer, probeCh, initial crawl)
// are dropped while a cycle is in progress.
func (m *multipleDNSTTTransport) tryAllDNSTunnels() {
	if !m.probing.CompareAndSwap(false, true) {
		slog.Debug("dnstt probe already in progress, skipping")
		return
	}
	defer m.probing.Store(false)

	if len(m.configs) == 0 {
		slog.Debug("no dns tunnel options available")
		return
	}
	if len(m.tunChan) >= maxWorkingTunnels {
		slog.Debug("enough working dns tunnels, skipping probe", slog.Int("working", len(m.tunChan)))
		return
	}

	slog.Debug("selecting dnstt options with active probing", slog.Int("options", len(m.configs)))

	pondCtx, cancel := context.WithTimeout(context.Background(), waitFor)
	m.probeCancelMx.Lock()
	m.probeCancelFn = cancel
	m.probeCancelMx.Unlock()
	defer cancel()

	poolSize := 10
	if len(m.configs) < poolSize {
		poolSize = len(m.configs)
	}

	pool := pond.New(poolSize, 10, pond.Context(pondCtx))
	start := m.probeCursor
	tested := 0
	for i := range m.configs {
		if len(m.tunChan) >= maxWorkingTunnels {
			break
		}
		cfg := m.configs[(start+i)%len(m.configs)]
		tested++
		pool.Submit(func() {
			if m.closed.Load() || len(m.tunChan) >= maxWorkingTunnels {
				return
			}

			// Instances are created here, not at startup, so only poolSize DNSTT
			// instances (and their goroutines) are active at any one time.
			resolver := cfgResolver(cfg)
			dnstImpl, err := newDNSTT(cfg)
			if err != nil {
				slog.Debug("failed to create dnstt instance", slog.String("domain", cfg.Domain), slog.String("resolver", resolver), slog.Any("error", err))
				return
			}
			tun := &dnsTunnel{DNSTT: dnstImpl, domain: cfg.Domain, resolver: resolver}

			rt, err := tun.NewRoundTripper(pondCtx, "")
			if err != nil {
				slog.Debug("failed to create round tripper", slog.String("domain", cfg.Domain), slog.String("resolver", resolver), slog.Any("error", err))
				tun.Close()
				return
			}

			// 180 s covers DNSTT session establishment (~20-60 s over DoH) and
			// TLS handshake through 135-byte MTU tunnel (multiple round trips).
			client := &http.Client{Transport: rt, Timeout: 180 * time.Second}

			req, err := http.NewRequestWithContext(pondCtx, http.MethodGet, "http://www.gstatic.com/generate_204", http.NoBody)
			if err != nil {
				slog.Debug("failed to create request", slog.String("domain", cfg.Domain), slog.String("resolver", resolver), slog.Any("error", err))
				tun.Close()
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				slog.Debug("dnstt probe failed", slog.String("domain", cfg.Domain), slog.String("resolver", resolver), slog.Any("error", err))
				tun.Close()
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				slog.Debug("dnstt probe returned non-2xx", slog.String("domain", cfg.Domain), slog.String("resolver", resolver), slog.Int("status", resp.StatusCode))
				tun.Close()
				return
			}

			slog.Debug("dnstt tunnel ready", slog.String("domain", cfg.Domain), slog.String("resolver", resolver))
			tun.markSucceeded()
			select {
			case m.tunChan <- tun:
			default:
				tun.Close()
			}
		})
	}
	m.probeCursor = (start + tested) % len(m.configs)
	pool.StopAndWaitFor(waitFor)
}

const probeInterval = 5 * time.Minute

// RequestTimeout returns the maximum time the race transport should wait for
// a single request through this DNSTT transport. DNSTT sessions are slow
// (DNS-tunneled, 135-byte MTU) — a single TLS handshake over the tunnel
// can take tens of seconds, so this budget is significantly longer than
// the race transport default.
func (m *multipleDNSTTTransport) RequestTimeout() time.Duration {
	return 5 * time.Minute
}

// MaxLength returns the maximum request body size this transport supports.
// DNS tunnels have a 135-byte MTU — a 10 KB body requires ~75 DNS queries
// which already pushes the RequestTimeout budget. Larger bodies are routed
// to a different transport.
func (m *multipleDNSTTTransport) MaxLength() int {
	return 10_000
}

type multipleDNSTTTransport struct {
	crawlOnce    sync.Once
	tunChan      chan *dnsTunnel
	stopChan     chan struct{}
	stopChanOnce sync.Once
	closed       atomic.Bool
	configs      []dnsttConfig

	// probeCh triggers an on-demand probe cycle when NewRoundTripper
	// exhausts all available tunnels. Buffered (1) so a trigger is
	// never lost but spurious duplicate triggers are dropped.
	probeCh chan struct{}

	// probeCancelFn cancels the pondCtx for the in-progress tryAllDNSTunnels
	// call. Stored so Close() can abort probe workers promptly, preventing
	// their DoH goroutines from competing with a subsequent transport's probe.
	probeCancelFn context.CancelFunc
	probeCancelMx sync.Mutex

	// probing single-flights tryAllDNSTunnels. The timer, probeCh, and the
	// initial crawl can all fire it concurrently; without this a second run
	// would overwrite probeCancelFn and leave the first run's workers
	// uncancellable.
	probing atomic.Bool

	// probeCursor is the config index the next probe cycle resumes from, so
	// successive cycles spread probing across all configs instead of always
	// re-testing the same prefix. Guarded by probing.
	probeCursor int
}

// maxWorkingTunnels caps how many established tunnels tunChan retains. Once
// reached, probing stops and resumes only to replace tunnels that drop out,
// keeping the number of open DNSTT sessions (and their goroutines) bounded.
const maxWorkingTunnels = 5

// maxTunnelFailures is the number of consecutive failures after which a
// tunnel is permanently discarded.  A single failure is often transient
// (e.g. TLS handshake timeout through the slow DNS tunnel), so we give
// the tunnel several chances before giving up.
const maxTunnelFailures = 5

type dnsTunnel struct {
	dnstt.DNSTT
	domain   string
	resolver string

	lastSucceeded       time.Time
	consecutiveFailures int
	mx                  sync.RWMutex
}

func (t *dnsTunnel) markSucceeded() {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.lastSucceeded = time.Now()
	t.consecutiveFailures = 0
}

func (t *dnsTunnel) recordFailure() {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.consecutiveFailures++
	if t.consecutiveFailures >= maxTunnelFailures {
		t.lastSucceeded = time.Time{}
	}
}

func (t *dnsTunnel) isSucceeding() bool {
	t.mx.RLock()
	defer t.mx.RUnlock()
	return t.lastSucceeded.After(time.Time{}) && t.consecutiveFailures < maxTunnelFailures
}

// NewRoundTripper creates a pre-connected HTTP round tripper for the given
// address. It blocks until a KCP session and smux stream are established so
// that the race transport can fairly compare connection latencies.
func (m *multipleDNSTTTransport) NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error) {
	for range 6 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.stopChan:
			return nil, errors.New("dnstt stopped")
		case tun := <-m.tunChan:
			if !tun.isSucceeding() {
				tun.Close()
				continue
			}
			// Any error here means the tunnel is unusable, so drop it and try
			// the next rather than closing it again or recording a failure.
			rt, err := tun.NewRoundTripper(ctx, addr)
			if err != nil {
				continue
			}
			select {
			case m.tunChan <- tun:
			default:
			}
			return &connectedRoundtripper{t: tun, rt: rt}, nil
		}
	}
	select {
	case m.probeCh <- struct{}{}:
	default:
	}
	return nil, errors.New("no working dnstt tunnels available")
}

type connectedRoundtripper struct {
	t  *dnsTunnel
	rt http.RoundTripper
}

func (c *connectedRoundtripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err != nil {
		c.t.recordFailure()
		slog.WarnContext(req.Context(), "dnstt roundtripper failed",
			"domain", c.t.domain, "resolver", c.t.resolver, "error", err)
		return nil, err
	}

	slog.DebugContext(req.Context(), "dnstt roundtripper succeeded",
		"domain", c.t.domain, "resolver", c.t.resolver)
	c.t.markSucceeded()
	return resp, nil
}

// Close releases resources and closes active sessions.
func (m *multipleDNSTTTransport) Close() error {
	m.closed.Store(true)
	m.stopChanOnce.Do(func() {
		close(m.stopChan)
	})
	m.probeCancelMx.Lock()
	if m.probeCancelFn != nil {
		m.probeCancelFn()
	}
	m.probeCancelMx.Unlock()
	// Probe workers close their own failed instances synchronously, so only successful tunnels land here.
	for {
		select {
		case tun := <-m.tunChan:
			go func() {
				if err := tun.Close(); err != nil {
					slog.Error("failed to close dns tunnel", slog.Any("error", err))
				}
			}()
		default:
			return nil
		}
	}
}
