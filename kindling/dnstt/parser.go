package dnstt

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "embed"

	"github.com/alitto/pond"
	"github.com/getlantern/dnstt"
	"github.com/getlantern/keepcurrent"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling/smart"
	"github.com/getlantern/radiance/traces"
	"github.com/goccy/go-yaml"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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
func DNSTTOptions(ctx context.Context, localConfigFilepath string, logger io.Writer) ([]kindling.Option, []func() error, error) {
	ctx, span := otel.Tracer(tracerName).Start(
		ctx,
		"DNSTTOptions",
	)
	defer span.End()
	client, err := smart.NewHTTPClientWithSmartTransport(logger, dnsttConfigURL)
	if err != nil {
		span.RecordError(err)
		slog.Error("couldn't create http client for fetching dnstt configs", slog.Any("error", err))
	}
	// starting config updater/fetcher
	dnsttConfigUpdate(ctx, localConfigFilepath, client)

	// parsing embedded configs and loading options
	options, err := parseDNSTTConfigs(embeddedConfig)
	if err != nil {
		return nil, nil, traces.RecordError(ctx, fmt.Errorf("failed to parse dnstt embedded config: %w", err))
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
	kindlingOptions, closeFuncs := selectDNSTTOptions(ctx, options)
	span.AddEvent("selected dns tunnels", trace.WithAttributes(attribute.Int("options", len(kindlingOptions))))
	return kindlingOptions, closeFuncs, nil
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

const maxDNSTTOptions = 10

var waitFor = 30 * time.Second

func selectDNSTTOptions(ctx context.Context, options []dnsttConfig) ([]kindling.Option, []func() error) {
	slog.Debug("selecting dnstt options with active probing", slog.Int("options", len(options)))

	if len(options) == 0 {
		return nil, nil
	}

	pondCtx, cancel := context.WithTimeout(ctx, waitFor)
	defer cancel()

	var (
		mu         sync.Mutex
		selected   []dnstt.DNSTT
		closeFuncs []func() error
		foundCount atomic.Int32
	)

	// Limit concurrency to something reasonable
	poolSize := 10
	if len(options) < poolSize {
		poolSize = len(options)
	}

	pool := pond.New(poolSize, len(options), pond.Context(pondCtx))
	for _, opt := range options {
		pool.Submit(func() {
			// Stop early if we already have enough
			if foundCount.Load() >= maxDNSTTOptions {
				return
			}

			dnst, err := newDNSTT(opt)
			if err != nil {
				slog.Debug("couldn't build dns tunnel", slog.Any("error", err))
				return
			}

			rt, err := dnst.NewRoundTripper(ctx, "")
			if err != nil {
				slog.Debug("failed to create round tripper", slog.Any("error", err))
				dnst.Close()
				return
			}

			client := &http.Client{
				Transport: rt,
				Timeout:   15 * time.Second,
			}

			reqCtx, cancelReq := context.WithTimeout(pondCtx, 15*time.Second)
			defer cancelReq()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://www.gstatic.com/generate_204", http.NoBody)
			if err != nil {
				dnst.Close()
				return
			}

			resp, err := client.Do(req)
			if err != nil {
				slog.Debug("dnstt test request failed", slog.Any("error", err))
				dnst.Close()
				return
			}
			resp.Body.Close()

			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				slog.Debug("dnstt test returned non-2xx", slog.Int("status", resp.StatusCode))
				dnst.Close()
				return
			}

			mu.Lock()
			defer mu.Unlock()
			// Successful tunnel
			if foundCount.Add(1) <= maxDNSTTOptions {
				if opt.DoHResolver != nil {
					slog.Debug("selected DOH", slog.String("resolver", *opt.DoHResolver))
				}
				if opt.DoTResolver != nil {
					slog.Debug("selected DOT", slog.String("resolver", *opt.DoTResolver))
				}
				selected = append(selected, dnst)
				closeFuncs = append(closeFuncs, dnst.Close)

				// Cancel remaining work once we have enough
				if foundCount.Load() >= maxDNSTTOptions {
					cancel()
				}
			} else {
				// Extra tunnel found after limit
				dnst.Close()
			}
		})
	}
	pool.StopAndWaitFor(waitFor)

	kindlingOptions := make([]kindling.Option, 0, len(selected))
	for _, opt := range selected {
		kindlingOptions = append(kindlingOptions, kindling.WithDNSTunnel(opt))
	}

	slog.Debug(
		"dnstt selection complete",
		slog.Int("selected", len(kindlingOptions)),
		slog.Int("available", len(options)),
	)

	return kindlingOptions, closeFuncs
}
