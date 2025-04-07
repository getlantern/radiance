/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

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
type ListenerFunc func(oldConfig, newConfig []byte) error

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
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(pollInterval time.Duration, httpClient *http.Client, user *user.User, dataDir string) *ConfigHandler {
	configPath := filepath.Join(dataDir, configFileName)
	ch := &ConfigHandler{
		config:                  eventual.NewValue(),
		stopC:                   make(chan struct{}),
		closeOnce:               &sync.Once{},
		configPath:              configPath,
		apiClient:               common.NewWebClient(httpClient),
		preferredServerLocation: atomic.Value{}, // initially, no preference
		// prepoulate the configListeners with the save listener
		configListeners: []ListenerFunc{
			func(_, newConfig []byte) error {
				if err := saveConfig(configPath, newConfig); err != nil {
					return fmt.Errorf("saving config: %w", err)
				}
				slog.Debug("saved config to disk")
				return nil
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

// ListAvailableServers returns a list of available servers from the current configuration.
// If no configuration is available, it returns an error.
func (ch *ConfigHandler) ListAvailableServers(ctx context.Context) ([]C.ServerLocation, error) {
	return nil, fmt.Errorf("not implemented")
}

// AddConfigListener adds a listener for new ConfigResponses.
func (ch *ConfigHandler) AddConfigListener(listener ListenerFunc) {
	ch.configListenersMu.Lock()
	ch.configListeners = append(ch.configListeners, listener)
	ch.configListenersMu.Unlock()
	// if we have a config already, call the listener with it
	if cfgRes, err := ch.config.Get(eventual.DontWait); err == nil {
		cfg, ok := cfgRes.([]byte)
		if ok && cfg != nil {
			listener(nil, cfg)
		}
	}
}

func (ch *ConfigHandler) notifyListeners(oldConfig, newConfig []byte) {
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
	oldConfig, err := ch.config.Get(eventual.DontWait)
	var oldConfigBytes []byte
	if oldConfig != nil {
		var ok bool
		oldConfigBytes, ok = oldConfig.([]byte)
		if !ok {
			slog.Error("failed to cast old config to bytes", "error", err)
		}
	}
	ch.config.Set(resp)
	ch.notifyListeners(oldConfigBytes, resp)
	slog.Debug("Config fetched")
	return nil
}

func (ch *ConfigHandler) setConfig(cfg []byte) {
	slog.Debug("Setting config")
	ch.config.Set(cfg)
	ch.notifyListeners(cfg, cfg)
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
	ch.setConfig(cfg)
	return nil
}

// loadConfig loads the config file from the disk. If the config file is not found, it returns
// nil.
func loadConfig(path string) ([]byte, error) {
	slog.Debug("reading config file at", "path", path)
	buf, err := os.ReadFile(path)
	slog.Debug("config file read")
	if os.IsNotExist(err) { // no config file
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	return buf, nil
}

// saveConfig saves the configuration to the disk.
func saveConfig(path string, cfg []byte) error {
	return os.WriteFile(path, cfg, 0o600)
}
