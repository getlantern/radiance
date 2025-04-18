/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
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

	C "github.com/getlantern/common"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user"

	"github.com/sagernet/sing/common/json"
)

const (
	configFileName = "proxy.conf"
)

var (

	// ErrFetchingConfig is returned by [ConfigHandler.GetConfig] when if there was an error
	// fetching the configuration.
	ErrFetchingConfig = errors.New("failed to fetch config")
)

// Config includes all configuration data from the Lantern API as well as any stored local preferences.
type Config struct {
	ConfigResponse    C.ConfigResponse
	PreferredLocation C.ServerLocation
}

// ListenerFunc is a function that is called when the configuration changes.
type ListenerFunc func(oldConfig, newConfig *Config) error

// Unmarshaller is a function that parses the configuration response from the server.
type Unmarshaller func(config []byte) (*Config, error)

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config    atomic.Value
	ftr       *fetcher
	apiClient common.WebClient
	stopC     chan struct{}
	closeOnce *sync.Once

	configPath        string
	configListeners   []ListenerFunc
	configListenersMu sync.RWMutex
	configParser      Unmarshaller
	configMu          sync.RWMutex
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(pollInterval time.Duration, httpClient *http.Client, user user.BaseUser, dataDir string,
	configParser Unmarshaller) *ConfigHandler {
	configPath := filepath.Join(dataDir, configFileName)
	ch := &ConfigHandler{
		config:          atomic.Value{},
		stopC:           make(chan struct{}),
		closeOnce:       &sync.Once{},
		configPath:      configPath,
		apiClient:       common.NewWebClient(httpClient),
		configListeners: make([]ListenerFunc, 0),
		configParser:    configParser,
	}

	if err := ch.loadConfig(); err != nil {
		slog.Error("failed to load config", "error", err)
	}

	ch.ftr = newFetcher(httpClient, user)
	go ch.fetchLoop(pollInterval)
	return ch
}

// SetPreferredServerLocation sets the preferred server location to connect to
func (ch *ConfigHandler) SetPreferredServerLocation(country, city string) {
	preferred := C.ServerLocation{
		Country: country,
		City:    city,
	}
	ch.modifyConfig(func(cfg *Config) {
		cfg.PreferredLocation = preferred
	})
	// fetch the config with the new preferred location on a separate goroutine
	go func() {
		if err := ch.fetchConfig(); err != nil {
			slog.Error("Failed to fetch config: %v", "error", err)
		}
	}()
}

// ListAvailableServers returns a list of available servers from the current configuration.
// If no configuration is available, it returns an error.
func (ch *ConfigHandler) ListAvailableServers() ([]C.ServerLocation, error) {
	cfgRes := ch.config.Load()
	if cfgRes == nil {
		return nil, fmt.Errorf("getting config: %w", ErrFetchingConfig)
	}
	cfg, ok := cfgRes.(*Config)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("config is not the expected type")
	}
	return cfg.ConfigResponse.Servers, nil
}

// AddConfigListener adds a listener for new Configs.
func (ch *ConfigHandler) AddConfigListener(listener ListenerFunc) {
	ch.configListenersMu.Lock()
	ch.configListeners = append(ch.configListeners, listener)
	ch.configListenersMu.Unlock()
	cfg, err := ch.GetConfig()
	if err != nil {
		slog.Error("getting config", "error", err)
		return
	}
	go func() {
		err := listener(nil, cfg)
		if err != nil {
			slog.Error("Listener error", "error", err)
		}
	}()
}

func (ch *ConfigHandler) notifyListeners(oldConfig, newConfig *Config) {
	ch.configListenersMu.RLock()
	defer ch.configListenersMu.RUnlock()
	for _, listener := range ch.configListeners {
		go func() {
			err := listener(oldConfig, newConfig)
			if err != nil {
				slog.Error("Listener error", "error", err)
			}
		}()
	}
}

func (ch *ConfigHandler) fetchConfig() error {
	slog.Debug("Fetching config")
	var preferredServerLocation C.ServerLocation
	oldConfig, err := ch.GetConfig()
	if err != nil {
		slog.Info("Config not available yet", "error", err)
	} else {
		preferredServerLocation = oldConfig.PreferredLocation
	}
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
	cfg, err := ch.configParser(resp)

	if err != nil {
		slog.Error("failed to parse config", "error", err)
		return fmt.Errorf("parsing config: %w", err)
	}
	ch.setConfigAndNotify(cfg)

	slog.Debug("Config fetched")
	return nil
}

func (ch *ConfigHandler) setConfigAndNotify(cfg *Config) {
	slog.Debug("Setting config")
	if cfg == nil {
		slog.Debug("Config is nil, not setting")
		return
	}
	// Lock config access
	ch.configMu.Lock()
	defer ch.configMu.Unlock()
	oldConfig, _ := ch.GetConfig()
	// Create a deep copy of the old config to avoid modifying it while merging
	if oldConfig != nil {
		oldConfigCopy := *oldConfig
		if err := mergo.Merge(&oldConfigCopy, cfg, mergo.WithOverride); err != nil {
			slog.Error("merging config", "error", err)
			return
		}
		cfg = &oldConfigCopy
	}

	ch.config.Store(cfg)
	ch.saveConfig(cfg)
	go ch.notifyListeners(oldConfig, cfg)
	slog.Debug("Config set")
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

// Stop stops the ConfigHandler from fetching new configurations.
func (ch *ConfigHandler) Stop() {
	ch.closeOnce.Do(func() {
		close(ch.stopC)
	})
}

// loadConfig loads the config file from the disk. If the config file is not found, it returns
// nil.
func (ch *ConfigHandler) loadConfig() error {
	slog.Debug("reading config file")
	buf, err := os.ReadFile(ch.configPath)
	slog.Debug("config file read")
	if os.IsNotExist(err) { // no config file
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	cfg, err := ch.configParser(buf)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	ch.setConfigAndNotify(cfg)
	return nil
}

// saveConfig saves the config to the disk. It creates the config file if it doesn't exist.
func (ch *ConfigHandler) saveConfig(cfg *Config) {
	slog.Debug("Saving config")
	if cfg == nil {
		slog.Debug("Config is nil, not saving")
		return
	}
	if err := os.MkdirAll(filepath.Dir(ch.configPath), 0o755); err != nil {
		slog.Error("creating config directory", "error", err)
		return
	}
	// Marshal the config to bytes
	// and write it to the config file.
	// If the config is nil, we don't write anything.
	// This is important because we don't want to overwrite the config file with an empty file.

	buf, err := json.Marshal(cfg)
	if err != nil {
		slog.Error("marshalling config", "error", err)
		return
	}
	if err := os.WriteFile(ch.configPath, buf, 0o600); err != nil {
		slog.Error("writing config file", "error", err)
	}
	slog.Debug("Config saved")
}

// GetConfig returns the current configuration. It returns an error if the config is not yet available.
func (ch *ConfigHandler) GetConfig() (*Config, error) {
	cfgRes := ch.config.Load()
	if cfgRes == nil {
		return nil, fmt.Errorf("no config")
	}
	cfg, ok := cfgRes.(*Config)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	return cfg, nil
}

// modifyConfig saves the config to the disk with the given config. It creates the config file
// if it doesn't exist.
func (ch *ConfigHandler) modifyConfig(fn func(cfg *Config)) {
	ch.configMu.Lock()
	cfg, err := ch.GetConfig()
	if err != nil {
		slog.Error("getting config", "error", err)
		ch.configMu.Unlock()
		return
	}
	// Call the function with the config
	// and save the config to the disk.
	fn(cfg)
	ch.configMu.Unlock()
	ch.setConfigAndNotify(cfg)
}
