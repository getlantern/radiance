/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"dario.cat/mergo"
	"github.com/getlantern/eventual/v2"

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user"
)

const (
	configFileName = "proxy.conf"
)

var (

	// ErrFetchingConfig is returned by [ConfigHandler.GetConfig] when if there was an error
	// fetching the configuration.
	ErrFetchingConfig = errors.New("failed to fetch config")
)

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config    eventual.Value
	ftr       *fetcher
	apiClient common.WebClient
	stopC     chan struct{}
	closeOnce *sync.Once

	configPath              string
	preferredServerLocation atomic.Value
	configListeners         []func(*C.ConfigResponse)
	configListenersMu       sync.RWMutex
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(pollInterval time.Duration, httpClient *http.Client, user *user.User, dataDir string) *ConfigHandler {
	ch := &ConfigHandler{
		config:                  eventual.NewValue(),
		stopC:                   make(chan struct{}),
		closeOnce:               &sync.Once{},
		configPath:              filepath.Join(dataDir, configFileName),
		apiClient:               common.NewWebClient(httpClient),
		preferredServerLocation: atomic.Value{}, // initially, no preference
		// prepoulate the configListeners with the save listener
		configListeners: []func(*C.ConfigResponse){
			func(cfg *C.ConfigResponse) {
				if err := saveConfig(dataDir, cfg); err != nil {
					slog.Error("failed to save config: %v", "error", err)
				}
			},
		},
	}
	// Store an empty preferred location to avoid nil pointer dereference
	ch.preferredServerLocation.Store(&C.ServerLocation{})

	if err := ch.loadConfig(); err != nil {
		slog.Error("failed to load config", "error", err)
	}

	ch.ftr = newFetcher(httpClient, user)
	go ch.fetchLoop(pollInterval)
	return ch
}

func (ch *ConfigHandler) SetPreferredServerLocation(country, city string) {
	ch.preferredServerLocation.Store(&C.ServerLocation{
		Country: country,
		City:    city,
	})

	// fetch the config with the new preferred location on a separate goroutine
	go func() {
		if err := ch.fetchConfig(); err != nil {
			slog.Error("Failed to fetch config: %v", "error", err)
		}
	}()
}

func (ch *ConfigHandler) ListAvailableServers(ctx context.Context) ([]C.ServerLocation, error) {
	cfg, err := ch.config.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting config: %w", err)
	}
	cfgRes := cfg.(*C.ConfigResponse)
	if cfgRes == nil {
		return nil, fmt.Errorf("config is nil")
	}

	availableServers := make([]C.ServerLocation, 0, len(cfgRes.Servers))
	return append(availableServers, cfgRes.Servers...), nil
}

// AddConfigListener adds a listener for new ConfigResponses.
func (ch *ConfigHandler) AddConfigListener(listener func(*C.ConfigResponse)) {
	ch.configListenersMu.Lock()
	ch.configListeners = append(ch.configListeners, listener)
	ch.configListenersMu.Unlock()
	// if we have a config already, call the listener with it
	if cfgRes, err := ch.config.Get(eventual.DontWait); err == nil {
		cfg, ok := cfgRes.(*C.ConfigResponse)
		if ok && cfg != nil {
			listener(cfg)
		}
	}
}

func (ch *ConfigHandler) notifyListeners(cfg *C.ConfigResponse) {
	ch.configListenersMu.RLock()
	defer ch.configListenersMu.RUnlock()
	for _, listener := range ch.configListeners {
		listener(cfg)
	}
}

func (ch *ConfigHandler) fetchConfig() error {
	slog.Debug("Fetching config")
	preferredServerLocation := ch.preferredServerLocation.Load().(*C.ServerLocation)
	resp, err := ch.ftr.fetchConfig(preferredServerLocation)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFetchingConfig, err)
	}
	if resp == nil {
		slog.Debug("no new config available")
		return nil
	}

	// Otherwise, we keep the previous config and store any error that might have occurred.
	// We still want to keep the previous config if there was an error. This is important
	// because the error could have been due to temporary network issues, such as brief
	// power loss or internet disconnection.
	// On the other hand, if we have a new config, we want to overwrite any previous error.

	ch.mergeConfig(resp)
	return nil
}

// mergeConfig merges the new config with the existing config. If the existing config is nil, it sets the new config.
// The new config overwrites any existing values in the old config.
// It returns an error if the merge fails.
func (ch *ConfigHandler) mergeConfig(cfg *C.ConfigResponse) error {
	slog.Debug("Merging config")
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	existingConfig, _ := ch.config.Get(eventual.DontWait)
	if existingConfig != nil {
		mergedConfig := existingConfig.(*C.ConfigResponse)
		if err := mergo.MergeWithOverwrite(mergedConfig, cfg); err != nil {
			slog.Error("merging config", "error", err)
			return fmt.Errorf("merging config: %w", err)
		}
		cfg = mergedConfig
	}
	ch.config.Set(cfg)
	ch.notifyListeners(cfg)
	slog.Debug("Config set")
	return nil
}

// fetchLoop fetches the configuration every pollInterval.
func (ch *ConfigHandler) fetchLoop(pollInterval time.Duration) {
	if err := ch.fetchConfig(); err != nil {
		slog.Error("Failed to fetch config. Retrying", "error", err, "interval", pollInterval)
	}
	for {
		select {
		case <-ch.stopC:
			return
		case <-time.After(pollInterval):
			if err := ch.fetchConfig(); err != nil {
				slog.Error("Failed to fetch config in select. Retrying", "error", err, "interval", pollInterval)
			}
		}
	}
}

// GetConfig returns the current proxy configuration and the country. If no configuration is available, GetConfig
// will wait until one is available or the context has expired. If an error occurred during the
// last fetch, that error is returned, as a ErrFetchingConfig, along with the most recent
// configuration, if available. GetConfig is a blocking call.
func (ch *ConfigHandler) GetConfig(ctx context.Context) (*C.ConfigResponse, error) {
	_cfgRes, err := ch.config.Get(ctx)
	if err != nil { // ctx expired
		return nil, fmt.Errorf("getting config: %w", err)
	}
	cfgRes := _cfgRes.(*C.ConfigResponse)
	return cfgRes, nil
}

// Stop stops the ConfigHandler from fetching new configurations.
func (ch *ConfigHandler) Stop() {
	ch.closeOnce.Do(func() {
		close(ch.stopC)
	})
}

// loadConfig loads the configuration from the disk and sets it in the ConfigHandler.
func (ch *ConfigHandler) loadConfig() error {
	slog.Debug("Loading config")
	cfg, err := loadConfig(ch.configPath)
	if err != nil {
		slog.Error("loading config", "error", err)
		err = fmt.Errorf("loading config: %w", err)
		return err
	}
	slog.Debug("Config loaded")
	if cfg == nil { // no config file
		slog.Debug("No config file found")
		return nil
	}
	slog.Debug("Setting config")
	ch.mergeConfig(cfg)
	return nil
}

// loadConfig loads the config file from the disk. If the config file is not found, it returns
// nil.
func loadConfig(path string) (*C.ConfigResponse, error) {
	slog.Debug("reading config file at", "path", path)
	buf, err := os.ReadFile(path)
	slog.Debug("config file read")
	if os.IsNotExist(err) { // no config file
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var cfg C.ConfigResponse
	err = json.Unmarshal(buf, &cfg)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}
	return &cfg, nil
}

// saveConfig saves the configuration to the disk.
func saveConfig(path string, cfg *C.ConfigResponse) error {
	buf, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, buf, 0o600)
}
