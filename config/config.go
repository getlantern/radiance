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
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	C "github.com/getlantern/common"
	"github.com/qdm12/reprint"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"

	exO "github.com/getlantern/sing-box-extensions/option"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/common"
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
type Unmarshaller func(configRaw []byte) (*C.ConfigResponse, error)

type Options struct {
	PollInterval     time.Duration
	HTTPClient       *http.Client
	User             common.UserInfo
	DataDir          string
	ConfigRespParser Unmarshaller
	Locale           string
}

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config    atomic.Value
	ftr       Fetcher
	apiClient common.WebClient
	stopC     chan struct{}
	closeOnce *sync.Once

	configPath        string
	configListeners   []ListenerFunc
	configListenersMu sync.RWMutex
	confRespParser    Unmarshaller
	configMu          sync.RWMutex

	// wgKeyPath is the path to the WireGuard private key file.
	wgKeyPath         string
	preferredLocation atomic.Value
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(options Options) *ConfigHandler {
	configPath := filepath.Join(options.DataDir, app.ConfigFileName)
	opts := common.WebClientOptions{
		BaseURL:    "",
		HttpClient: options.HTTPClient,
	}
	ch := &ConfigHandler{
		config:          atomic.Value{},
		stopC:           make(chan struct{}),
		closeOnce:       &sync.Once{},
		configPath:      configPath,
		apiClient:       common.NewWebClient(&opts),
		configListeners: make([]ListenerFunc, 0),
		confRespParser:  options.ConfigRespParser,
		wgKeyPath:       filepath.Join(options.DataDir, "wg.key"),
	}

	// Set the preferred location to an empty struct to define the underlying type.
	ch.preferredLocation.Store(C.ServerLocation{})

	if err := ch.loadConfig(); err != nil {
		slog.Error("failed to load config", "error", err)
	}

	ch.ftr = newFetcher(options.HTTPClient, options.User, options.Locale)
	go ch.fetchLoop(options.PollInterval)
	return ch
}

var ErrNoWGKey = errors.New("no wg key")

func (ch *ConfigHandler) loadWGKey() (wgtypes.Key, error) {
	buf, err := os.ReadFile(ch.wgKeyPath)
	if os.IsNotExist(err) {
		return wgtypes.Key{}, ErrNoWGKey
	}
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("reading wg key file: %w", err)
	}
	key, err := wgtypes.ParseKey(string(buf))
	if err != nil {
		return wgtypes.Key{}, fmt.Errorf("parsing wg key: %w", err)
	}
	return key, nil
}

// SetPreferredServerLocation sets the preferred server location to connect to
func (ch *ConfigHandler) SetPreferredServerLocation(country, city string) {
	preferred := C.ServerLocation{
		Country: country,
		City:    city,
	}
	// We store the preferred location in memory in case we haven't fetched a config yet.
	ch.preferredLocation.Store(preferred)
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
		// There is no config yet, so we can't notify the listener.
		slog.Info("getting config", "error", err)
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
	var preferred C.ServerLocation
	oldConfig, err := ch.GetConfig()
	if err != nil {
		slog.Info("No stored config yet -- using in-memory server location", "error", err)
		storedLocation := ch.preferredLocation.Load()
		if storedLocation != nil {
			preferred = storedLocation.(C.ServerLocation)
		}
	} else {
		preferred = oldConfig.PreferredLocation
	}

	privateKey, err := ch.loadWGKey()
	if err != nil && !errors.Is(err, ErrNoWGKey) {
		return fmt.Errorf("loading wg key: %w", err)
	}

	if errors.Is(err, ErrNoWGKey) {
		privateKey, err = wgtypes.GeneratePrivateKey()
		if err != nil {
			return fmt.Errorf("failed to generate wg keys: %w", err)
		}

		if err := os.WriteFile(ch.wgKeyPath, []byte(privateKey.String()), 0o600); err != nil {
			return fmt.Errorf("writing wg key file: %w", err)
		}
	}

	resp, err := ch.ftr.fetchConfig(preferred, privateKey.PublicKey().String())
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
	confResp, err := ch.confRespParser(resp)

	if err != nil {
		slog.Error("failed to parse config", "error", err)
		return fmt.Errorf("parsing config: %w", err)
	}

	if err = settingWGPrivateKeyInConfig(confResp.Options.Endpoints, privateKey); err != nil {
		slog.Error("failed to replace private key", slog.Any("error", err))
		return fmt.Errorf("setting wireguard private key: %w", err)
	}
	ch.setConfigAndNotify(&Config{ConfigResponse: *confResp})

	slog.Debug("Config fetched")
	return nil
}

func settingWGPrivateKeyInConfig(endpoints []option.Endpoint, privateKey wgtypes.Key) error {
	for _, endpoint := range endpoints {
		switch opts := endpoint.Options.(type) {
		case *option.WireGuardEndpointOptions:
			opts.PrivateKey = privateKey.String()
		case *exO.AmneziaWGEndpointOptions:
			opts.PrivateKey = privateKey.String()
		default:
		}
	}
	return nil
}

func (ch *ConfigHandler) setConfigAndNotify(cfg *Config) {
	slog.Debug("Setting config")
	if cfg == nil {
		slog.Debug("Config is nil, not setting")
		return
	}
	oldConfig, _ := ch.GetConfig()
	if oldConfig != nil {
		merged, err := mergeResp(&oldConfig.ConfigResponse, &cfg.ConfigResponse)
		if err != nil {
			slog.Error("failed to merge config", "error", err)
			return
		}
		cfg.ConfigResponse = *merged

		if cfg.PreferredLocation == (C.ServerLocation{}) {
			cfg.PreferredLocation = ch.preferredLocation.Load().(C.ServerLocation)
		}
	}

	ch.config.Store(cfg)
	if err := saveConfig(cfg, ch.configPath); err != nil {
		slog.Error("saving config", "error", err)
		return
	}
	slog.Debug("saved new config")
	go ch.notifyListeners(oldConfig, cfg)
	slog.Debug("Config set")
}

// mergeResp merges the old and new configuration responses. The merged response is returned
// along with any error that occurred during the merge.
func mergeResp(oldConfig, newConfig *C.ConfigResponse) (*C.ConfigResponse, error) {
	oldConfigCopy := reprint.This(*oldConfig).(C.ConfigResponse)
	if err := mergo.Merge(&oldConfigCopy, newConfig, mergo.WithOverride); err != nil {
		slog.Error("merging config", "error", err)
		return newConfig, err
	}
	return &oldConfigCopy, nil
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
	cfg, err := ch.unmarshalConfig(buf)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	ch.config.Store(cfg)
	go ch.notifyListeners(nil, cfg)
	return nil
}

func (ch *ConfigHandler) unmarshalConfig(data []byte) (*Config, error) {
	type T struct {
		ConfigResponse    json.RawMessage
		PreferredLocation C.ServerLocation
	}
	var tmp T
	if err := json.Unmarshal(data, &tmp); err != nil {
		return nil, err
	}
	opts, err := ch.confRespParser(tmp.ConfigResponse)
	if err != nil {
		return nil, err
	}
	return &Config{
		ConfigResponse:    *opts,
		PreferredLocation: tmp.PreferredLocation,
	}, nil
}

// saveConfig saves the config to the disk. It creates the config file if it doesn't exist.
func saveConfig(cfg *Config, path string) error {
	slog.Debug("Saving config")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		slog.Error("creating config directory", "error", err)
		return fmt.Errorf("creating config directory: %w", err)
	}
	// Marshal the config to bytes and write it to the config file.
	// If the config is nil, we don't write anything.
	// This is important because we don't want to overwrite the config file with an empty file.
	buf, err := singjson.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	return os.WriteFile(path, buf, 0o600)
}

// GetConfig returns the current configuration. It returns an error if the config is not yet available.
func (ch *ConfigHandler) GetConfig() (*Config, error) {
	cfg := ch.config.Load()
	if cfg == nil {
		return nil, fmt.Errorf("no config yet -- first run?")
	}
	return cfg.(*Config), nil
}

// modifyConfig saves the config to the disk with the given config. It creates the config file
// if it doesn't exist.
func (ch *ConfigHandler) modifyConfig(fn func(cfg *Config)) {
	ch.configMu.Lock()
	cfg, err := ch.GetConfig()
	if err != nil {
		// This could happen if we haven't successfully fetched the config yet.
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
