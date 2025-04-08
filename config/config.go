/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
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

// ListenerFunc is a function that is called when the configuration changes.
type ListenerFunc func(oldConfig, newConfig *C.ConfigResponse) error

// ConfigParser is a function that parses the configuration response from the server.
type ConfigParser func(config []byte) (*C.ConfigResponse, error)

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
	configListeners         []ListenerFunc
	configListenersMu       sync.RWMutex
	configParser            ConfigParser
	configMu                sync.RWMutex
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(pollInterval time.Duration, httpClient *http.Client, user *user.User, dataDir string,
	configParser ConfigParser) *ConfigHandler {
	configPath := filepath.Join(dataDir, configFileName)
	ch := &ConfigHandler{
		config:                  eventual.NewValue(),
		stopC:                   make(chan struct{}),
		closeOnce:               &sync.Once{},
		configPath:              configPath,
		apiClient:               common.NewWebClient(httpClient),
		preferredServerLocation: atomic.Value{}, // initially, no preference
		configListeners:         make([]ListenerFunc, 0),
		configParser:            configParser,
	}
	// Store an empty preferred location to avoid nil pointer dereference
	ch.preferredServerLocation.Store(C.ServerLocation{})

	if err := ch.loadConfig(); err != nil {
		slog.Error("failed to load config", "error", err)
	}

	ch.ftr = newFetcher(httpClient, user)
	go ch.fetchLoop(pollInterval)
	return ch
}

func (ch *ConfigHandler) SetPreferredServerLocation(country, city string) {
	ch.preferredServerLocation.Store(C.ServerLocation{
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

// ListAvailableServers returns a list of available servers from the current configuration.
// If no configuration is available, it returns an error.
func (ch *ConfigHandler) ListAvailableServers() ([]C.ServerLocation, error) {
	cfgRes, err := ch.config.Get(eventual.DontWait)
	if err != nil {
		return nil, fmt.Errorf("getting config: %w", err)
	}
	cfg, ok := cfgRes.(*C.ConfigResponse)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	return cfg.Servers, nil
}

// AddConfigListener adds a listener for new ConfigResponses.
func (ch *ConfigHandler) AddConfigListener(listener ListenerFunc) {
	ch.configListenersMu.Lock()
	ch.configListeners = append(ch.configListeners, listener)
	ch.configListenersMu.Unlock()
	// if we have a config already, call the listener with it
	if cfgRes, err := ch.config.Get(eventual.DontWait); err == nil {
		cfg, ok := cfgRes.(*C.ConfigResponse)
		if ok && cfg != nil {
			go func() {
				err := listener(nil, cfg)
				if err != nil {
					slog.Error("Listener error", "error", err)
				}
			}()
		}
	}
}

func (ch *ConfigHandler) notifyListeners(oldConfig, newConfig *C.ConfigResponse) {
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
	preferredServerLocation := ch.preferredServerLocation.Load().(C.ServerLocation)
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
	ch.setConfig(cfg)

	slog.Debug("Config fetched")
	return nil
}

func (ch *ConfigHandler) setConfig(cfg *C.ConfigResponse) {
	slog.Debug("Setting config")
	if cfg == nil {
		slog.Debug("Config is nil, not setting")
		return
	}
	// Lock config access
	ch.configMu.Lock()
	defer ch.configMu.Unlock()
	var oldConfig *C.ConfigResponse
	oldConfigRaw, _ := ch.config.Get(eventual.DontWait)
	if oldConfigRaw != nil {
		oldConfig = oldConfigRaw.(*C.ConfigResponse)
	}
	// Create a deep copy of the old config to avoid modifying it while merging
	if oldConfig != nil {
		oldConfigCopy := *oldConfig
		if err := mergo.Merge(&oldConfigCopy, cfg, mergo.WithOverride); err != nil {
			slog.Error("merging config", "error", err)
			return
		}
		cfg = &oldConfigCopy
	}

	ch.config.Set(cfg)
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
	ch.setConfig(cfg)
	return nil
}

// saveConfig saves the config to the disk. It creates the config file if it doesn't exist.
func (ch *ConfigHandler) saveConfig(cfg *C.ConfigResponse) {
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

// GetConfig returns the current configuration. It blocks until the configuration is available.
func (ch *ConfigHandler) GetConfig() (*C.ConfigResponse, error) {
	cfgRes, err := ch.config.Get(eventual.DontWait)
	if err != nil {
		return nil, fmt.Errorf("getting config: %w", err)
	}
	cfg, ok := cfgRes.(*C.ConfigResponse)
	if !ok || cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	return cfg, nil
}
