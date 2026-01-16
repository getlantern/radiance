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
	"time"

	_ "embed"

	"github.com/getlantern/dnstt"
	"github.com/getlantern/keepcurrent"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/events"
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

func DNSTTOptions(ctx context.Context, localConfigFilepath string, client *http.Client) ([]kindling.Option, error) {
	options, err := parseDNSTTConfigs(embeddedConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to parse dnstt embedded config: %w", err)
	}

	if localConfigFilepath != "" {
		localConfigMutex.Lock()
		defer localConfigMutex.Unlock()
		if config, err := os.ReadFile(localConfigFilepath); err == nil {
			opts, err := parseDNSTTConfigs(config)
			if err != nil {
				slog.Warn("failed to parse local dnstt config, returning embedded dnstt config", slog.Any("error", err))
				return options, nil
			}
			return opts, nil
		} else {
			slog.Warn("failed to read local dnstt config file", slog.Any("error", err), slog.String("filepath", localConfigFilepath))
		}
	}
	dnsttConfigUpdate(ctx, localConfigFilepath, client)
	return options, nil
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
	closeChan := sync.OnceFunc(func() {
		close(chDB)
	})
	dest := keepcurrent.ToChannel(chDB)

	runner := keepcurrent.NewWithValidator(
		dnsttConfigValidator(),
		source,
		dest,
	)

	go func() {
		for {
			select {
			case <-ctx.Done():
				closeChan()
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

	runner.Start(pollInterval)
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

func parseDNSTTConfigs(gzipyml []byte) ([]kindling.Option, error) {
	cfgs, err := processYaml(gzipyml)
	if err != nil {
		return nil, err
	}

	options := make([]kindling.Option, 0)
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
		options = append(options, kindling.WithDNSTunnel(d))
	}

	return options, nil
}
