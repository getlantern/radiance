package config

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/getlantern/dnstt"
	"github.com/getlantern/keepcurrent"
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

func processYaml(gzippedYaml []byte) (dnsttConfig, error) {
	r, gzipErr := gzip.NewReader(bytes.NewReader(gzippedYaml))
	if gzipErr != nil {
		return dnsttConfig{}, fmt.Errorf("failed to create gzip reader: %w", gzipErr)
	}
	defer r.Close()
	yml, err := io.ReadAll(r)
	if err != nil {
		return dnsttConfig{}, fmt.Errorf("failed to read gzipped file: %w", err)
	}
	path, err := yaml.PathString("$.dnstt")
	if err != nil {
		return dnsttConfig{}, fmt.Errorf("failed to create config path: %w", err)
	}
	var cfg dnsttConfig
	if err = path.Read(bytes.NewReader(yml), &cfg); err != nil {
		return dnsttConfig{}, fmt.Errorf("failed to read config: %w", err)
	}

	return cfg, nil
}

type NewDNSTTConfigEvent struct {
	events.Event
	New dnstt.DNSTT
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

func DNSTTConfigUpdate(ctx context.Context, configURL string, httpClient *http.Client, pollInterval time.Duration) {
	if configURL == "" {
		slog.Debug("No config URL provided -- not updating dnstt configuration")
		return
	}

	slog.Debug("Updating dnstt configuration", slog.String("url", configURL))
	source := keepcurrent.FromWebWithClient(configURL, httpClient)
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
				if err := onNewDNSTTConfig(data); err != nil {
					slog.Error("failed to apply new dnstt configuration", "error", err)
				}
			}
		}
	}()

	runner.Start(pollInterval)
}

func onNewDNSTTConfig(gzippedYML []byte) error {
	cfg, err := processYaml(gzippedYML)
	if err != nil {
		return fmt.Errorf("failed to process dnstt config: %w", err)
	}

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
		return fmt.Errorf("failed to build new dnstt: %w", err)
	}

	slog.Info("applied new dnstt configuration", slog.Any("config", cfg))
	events.Emit(NewDNSTTConfigEvent{
		New: d,
	})

	return nil
}
