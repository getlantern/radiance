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
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling/smart"
	"github.com/getlantern/radiance/traces"
	"github.com/goccy/go-yaml"
	"go.opentelemetry.io/otel"
)

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
	tunnels := make([]dnstt.DNSTT, 0)
	for _, opt := range options {
		dnst, err := newDNSTT(opt)
		if err != nil {
			slog.Warn("failed to build dnstt", slog.Any("error", err))
			continue
		}

		tunnels = append(tunnels, dnst)
	}

	m := &multipleDNSTTTransport{
		tunChan:  make(chan *tun, 400),
		stopChan: make(chan struct{}),
		options:  tunnels,
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
	return os.WriteFile(configFilepath, gzippedYML, 0644)
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

var waitFor = 30 * time.Second

func (m *multipleDNSTTTransport) findWorkingDNSTunnels() {
	for {
		if len(m.tunChan) < 2 {
			m.tryAllDNSTunnels()
			time.Sleep(60 * time.Second)
		} else {
			select {
			case <-m.stopChan:
				slog.Debug("stopping parallel dialing dns tunnels")
				return
			case <-time.After(30 * time.Minute):
				// Run again after a random time between 5 min
			}
		}
	}
}

func (m *multipleDNSTTTransport) tryAllDNSTunnels() {
	slog.Debug("selecting dnstt options with active probing", slog.Int("options", len(m.options)))

	if len(m.options) == 0 {
		slog.Debug("no dns tunnel options available")
		return
	}

	pondCtx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	// Limit concurrency to something reasonable
	poolSize := 10
	if len(m.options) < poolSize {
		poolSize = len(m.options)
	}

	pool := pond.New(poolSize, 10, pond.Context(pondCtx))
	for _, dnst := range m.options {
		pool.Submit(func() {
			if m.closed.Load() {
				slog.Debug("closed, stop testing")
				go dnst.Close()
				return
			}
			rt, err := dnst.NewRoundTripper(pondCtx, "")
			if err != nil {
				slog.Debug("failed to create round tripper", slog.Any("error", err))
				go dnst.Close()
				return
			}

			client := &http.Client{
				Transport: rt,
				Timeout:   15 * time.Second,
			}

			req, err := http.NewRequestWithContext(pondCtx, http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
			if err != nil {
				slog.Debug("failed to create request", slog.Any("error", err))
				go dnst.Close()
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				slog.Debug("dnstt test request failed", slog.Any("error", err))
				go dnst.Close()
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				slog.Debug("dnstt test returned non-2xx", slog.Int("status", resp.StatusCode))
				go dnst.Close()
				return
			}

			if m.closed.Load() {
				slog.Debug("closed, stop testing")
				go dnst.Close()
				return
			}

			// Successful tunnel
			slog.Debug("adding successful tun to channel")
			m.tunChan <- &tun{DNSTT: dnst, lastSucceeded: time.Now()}
		})
	}
	pool.StopAndWaitFor(waitFor)
}

type multipleDNSTTTransport struct {
	crawlOnce sync.Once
	tunChan   chan *tun
	stopChan  chan struct{}
	closed    atomic.Bool
	options   []dnstt.DNSTT
}

type tun struct {
	dnstt.DNSTT
	// lastSucceeded: the most recent time at which this DNS tunnel succeeded
	lastSucceeded time.Time
	mx            sync.RWMutex
}

func (t *tun) markSucceeded() {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.lastSucceeded = time.Now()
}

func (t *tun) markFailed() {
	t.mx.Lock()
	defer t.mx.Unlock()
	t.lastSucceeded = time.Time{}
}

func (t *tun) isSucceeding() bool {
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
	t    *tun
	ctx  context.Context
	addr string
}

func (c *connectedRoundtripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt, err := c.t.NewRoundTripper(c.ctx, c.addr)
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
	close(m.stopChan)
	for _, dnst := range m.options {
		go func() {
			if err := dnst.Close(); err != nil {
				slog.Error("failed to close dns tunnel", slog.Any("error", err))
			}
		}()
	}
	return nil
}
