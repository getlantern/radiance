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
		tunChan:  make(chan *dnsTunnel, 400),
		stopChan: make(chan struct{}),
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
	// trying all dns tunnels available
	go m.tryAllDNSTunnels()
	for {
		select {
		case <-m.stopChan:
			slog.Debug("stopping parallel dialing dns tunnels")
			return
		case <-time.After(30 * time.Minute):
			m.tryAllDNSTunnels()
		}
	}
}

func (m *multipleDNSTTTransport) tryAllDNSTunnels() {
	slog.Debug("selecting dnstt options with active probing", slog.Int("options", len(m.configs)))

	if len(m.configs) == 0 {
		slog.Debug("no dns tunnel options available")
		return
	}

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
	for _, cfg := range m.configs {
		cfg := cfg
		pool.Submit(func() {
			if m.closed.Load() {
				slog.Debug("closed, stop testing")
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
			tun := &dnsTunnel{DNSTT: dnstImpl}

			rt, err := tun.NewRoundTripper(pondCtx, "")
			if err != nil {
				slog.Debug("failed to create round tripper", slog.String("domain", cfg.Domain), slog.String("resolver", resolver), slog.Any("error", err))
				tun.Close()
				return
			}

			// 60 s covers DNSTT session establishment (~20-60 s over DoH) while
			// still failing fast enough for unreachable resolvers (TCP timeout ≈30 s).
			client := &http.Client{Transport: rt, Timeout: 60 * time.Second}

			req, err := http.NewRequestWithContext(pondCtx, http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
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

			if m.closed.Load() {
				slog.Debug("closed, stop testing")
				tun.Close()
				return
			}

			slog.Debug("dnstt tunnel ready", slog.String("domain", cfg.Domain), slog.String("resolver", resolver))
			tun.setProbeRT(rt)
			tun.markSucceeded()
			if !m.closed.Load() {
				m.tunChan <- tun
			} else {
				tun.Close()
			}
		})
	}
	pool.StopAndWaitFor(waitFor)
}

type multipleDNSTTTransport struct {
	crawlOnce    sync.Once
	tunChan      chan *dnsTunnel
	stopChan     chan struct{}
	stopChanOnce sync.Once
	closed       atomic.Bool
	configs      []dnsttConfig

	// probeCancelFn cancels the pondCtx for the in-progress tryAllDNSTunnels
	// call. Stored so Close() can abort probe workers promptly, preventing
	// their DoH goroutines from competing with a subsequent transport's probe.
	probeCancelFn context.CancelFunc
	probeCancelMx sync.Mutex
}

type dnsTunnel struct {
	dnstt.DNSTT
	lastSucceeded time.Time
	mx            sync.RWMutex

	// probeRT caches the round tripper established during the probe.
	// Reusing it avoids opening a second CONNECT tunnel on the same smux
	// session, which fails with 502 when the server limits concurrent
	// outbound connections per session.
	probeRT   http.RoundTripper
	probeRTMx sync.Mutex
}

func (t *dnsTunnel) setProbeRT(rt http.RoundTripper) {
	t.probeRTMx.Lock()
	defer t.probeRTMx.Unlock()
	t.probeRT = rt
}

func (t *dnsTunnel) getRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error) {
	t.probeRTMx.Lock()
	rt := t.probeRT
	t.probeRTMx.Unlock()
	if rt != nil {
		return rt, nil
	}
	return t.NewRoundTripper(ctx, addr)
}

func (t *dnsTunnel) markSucceeded() {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.lastSucceeded = time.Now()
}

func (t *dnsTunnel) markFailed() {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.lastSucceeded = time.Time{}
}

func (t *dnsTunnel) isSucceeding() bool {
	t.mx.RLock()
	defer t.mx.RUnlock()
	return t.lastSucceeded.After(time.Time{})
}

// NewRoundTripper creates a new HTTP round tripper for the given address.
// It manages session creation and reuse.
func (m *multipleDNSTTTransport) NewRoundTripper(ctx context.Context, addr string) (http.RoundTripper, error) {
	for range 6 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		// Add a case for the stop channel being called
		case <-m.stopChan:
			return nil, errors.New("dnstt stopped")
		case tun := <-m.tunChan:
			// The tun may have stopped succeeding since we last checked,
			// so only return it if it's still succeeding.
			if !tun.isSucceeding() {
				continue
			}

			// Add the tun back to the channel.
			m.tunChan <- tun
			return &connectedRoundtripper{t: tun, ctx: ctx, addr: addr}, nil
		}
	}
	return nil, fmt.Errorf("could not connect to any dns tunnel")
}

type connectedRoundtripper struct {
	t    *dnsTunnel
	ctx  context.Context
	addr string
}

func (c *connectedRoundtripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt, err := c.t.getRoundTripper(c.ctx, c.addr)
	if err != nil {
		slog.DebugContext(c.ctx, "failed to create dnstt round tripper", slog.Any("error", err))
		c.t.markFailed()
		return nil, err
	}

	resp, err := rt.RoundTrip(req)
	if err != nil {
		slog.WarnContext(c.ctx, "dnstt roundtripper failed", slog.Any("error", err))
		c.t.markFailed()
		return nil, err
	}

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
