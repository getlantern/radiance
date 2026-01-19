package dnstt

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"

	_ "embed"

	"github.com/getlantern/dnstt"
	"github.com/getlantern/keepcurrent"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling/smart"
	"github.com/goccy/go-yaml"
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

// DNSTTOptions load the embedded DNSTT config and return kindling options so
// it can be used as one of the transport options. If the local config filepath
// is provided and exists, this config will be loaded and if successfully
// parsed, will be returned instead of the embedded config.
func DNSTTOptions(ctx context.Context, localConfigFilepath string, logger io.Writer) ([]kindling.Option, []func() error, error) {
	client, err := smart.NewHTTPClientWithSmartTransport(logger, dnsttConfigURL)
	if err != nil {
		slog.Error("couldn't create http client for fetching dnstt configs", slog.Any("error", err))
	}
	// starting config updater/fetcher
	dnsttConfigUpdate(ctx, localConfigFilepath, client)

	// parsing embedded configs and loading options
	options, err := parseDNSTTConfigs(embeddedConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse dnstt embedded config: %w", err)
	}

	// if local config is set and exists, parse, load the dnstt config and close the embedded dns tunnels
	if localConfigFilepath != "" {
		localConfigMutex.Lock()
		defer localConfigMutex.Unlock()
		if config, err := os.ReadFile(localConfigFilepath); err == nil {
			opts, err := parseDNSTTConfigs(config)
			if err == nil {
				kindlingOptions, closeFuncs := selectDNSTTOptions(opts)
				return kindlingOptions, closeFuncs, nil
			}
			slog.Warn("failed to parse local dnstt config, returning embedded dnstt config", slog.Any("error", err))
		} else {
			slog.Warn("failed to read local dnstt config file", slog.Any("error", err), slog.String("filepath", localConfigFilepath))
		}
	}
	kindlingOptions, closeFuncs := selectDNSTTOptions(options)
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

func parseDNSTTConfigs(gzipyml []byte) ([]dnstt.DNSTT, error) {
	cfgs, err := processYaml(gzipyml)
	if err != nil {
		return nil, err
	}

	options := make([]dnstt.DNSTT, 0)
	for _, cfg := range cfgs {
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
		options = append(options, d)
	}

	return options, nil
}

const maxDNSTTOptions = 10

func selectDNSTTOptions(options []dnstt.DNSTT) ([]kindling.Option, []func() error) {
	slog.Debug("selecting dnstt options", slog.Int("options", len(options)))
	if len(options) == 0 {
		return []kindling.Option{}, []func() error{}
	}
	kindlingOptions := make([]kindling.Option, 0)
	closeDNSTTTunnel := make([]func() error, 0)

	if len(options) < maxDNSTTOptions {
		slog.Debug("options len is lower than max dnstt options, returning all options available", slog.Int("options", len(options)))
		for _, opt := range options {
			kindlingOptions = append(kindlingOptions, kindling.WithDNSTunnel(opt))
			closeDNSTTTunnel = append(closeDNSTTTunnel, opt.Close)
		}

		return kindlingOptions, closeDNSTTTunnel
	}

	slog.Debug("selecting random options", slog.Int("options", len(options)))
	for randomIndex := range generateRandomIndex(len(options), maxDNSTTOptions) {
		kindlingOptions = append(kindlingOptions, kindling.WithDNSTunnel(options[randomIndex]))
		closeDNSTTTunnel = append(closeDNSTTTunnel, options[randomIndex].Close)
	}
	return kindlingOptions, closeDNSTTTunnel
}

func generateRandomIndex(maxVal int, length int) map[int]struct{} {
	selectedIndexes := make(map[int]struct{})
	for len(selectedIndexes) < length {
		slog.Debug("generating n", slog.Int("maxVal", maxVal), slog.Int("len", length))
		selectedIndexes[rand.Intn(maxVal)] = struct{}{}
	}
	return selectedIndexes
}
