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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	C "github.com/getlantern/common"
	"github.com/sagernet/sing-box/option"
	singjson "github.com/sagernet/sing/common/json"

	box "github.com/getlantern/lantern-box"
	lbO "github.com/getlantern/lantern-box/option"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/servers"
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

type ServerManager interface {
	SetServers(serverGroup string, opts servers.Options) error
}

// ListenerFunc is a function that is called when the configuration changes.
type ListenerFunc func(oldConfig, newConfig *Config) error

type Options struct {
	PollInterval time.Duration
	HTTPClient   *http.Client
	SvrManager   ServerManager
	User         common.UserInfo
	DataDir      string
	Locale       string
	APIHandler   *api.APIClient
}

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config        atomic.Value
	ftr           Fetcher
	stopC         chan struct{}
	closeOnce     *sync.Once
	fetchDisabled bool

	configPath string
	configMu   sync.RWMutex

	svrManager ServerManager

	// wgKeyPath is the path to the WireGuard private key file.
	wgKeyPath         string
	preferredLocation atomic.Pointer[C.ServerLocation]
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(options Options) *ConfigHandler {
	configPath := filepath.Join(options.DataDir, common.ConfigFileName)
	ch := &ConfigHandler{
		config:        atomic.Value{},
		stopC:         make(chan struct{}),
		closeOnce:     &sync.Once{},
		fetchDisabled: options.PollInterval <= 0,
		configPath:    configPath,
		wgKeyPath:     filepath.Join(options.DataDir, "wg.key"),
		svrManager:    options.SvrManager,
	}
	// Set the preferred location to an empty struct to define the underlying type.
	ch.preferredLocation.Store(&C.ServerLocation{})

	if err := os.MkdirAll(filepath.Dir(options.DataDir), 0o755); err != nil {
		slog.Error("creating config directory", "error", err)
	}

	if err := ch.loadConfig(); err != nil {
		slog.Error("failed to load config", "error", err)
	}

	if !ch.fetchDisabled {
		ch.ftr = newFetcher(options.HTTPClient, options.User, options.Locale, options.APIHandler)
		go ch.fetchLoop(options.PollInterval)
		events.Subscribe(func(evt common.UserChangeEvent) {
			if !shouldRefetch(evt.New, evt.Old) {
				return
			}
			if err := ch.fetchConfig(); err != nil {
				slog.Error("Failed to fetch config", "error", err)
			}
		})
	}
	return ch
}

// shouldRefetch determines whether a config refetch is needed based on user ID and account type changes.
func shouldRefetch(new, old common.UserInfo) bool {
	return new != old &&
		(new == nil || old == nil || // sanity check
			new.LegacyToken() != old.LegacyToken() || // user ID changed
			new.AccountType() != old.AccountType()) // changed between free and pro
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
	preferred := &C.ServerLocation{
		Country: country,
		City:    city,
	}
	// We store the preferred location in memory in case we haven't fetched a config yet.
	ch.preferredLocation.Store(preferred)
	ch.modifyConfig(func(cfg *Config) {
		cfg.PreferredLocation = *preferred
	})
	// fetch the config with the new preferred location on a separate goroutine
	go func() {
		if err := ch.fetchConfig(); err != nil {
			slog.Error("Failed to fetch config: %v", "error", err)
		}
	}()
}

func (ch *ConfigHandler) fetchConfig() error {
	if ch.fetchDisabled {
		return fmt.Errorf("fetching config is disabled")
	}
	slog.Debug("Fetching config")
	var preferred C.ServerLocation
	oldConfig, err := ch.GetConfig()
	if err != nil {
		slog.Info("No stored config yet -- using in-memory server location", "error", err)
		storedLocation := ch.preferredLocation.Load()
		if storedLocation != nil {
			preferred = *storedLocation
		}
	} else {
		preferred = oldConfig.PreferredLocation
	}

	privateKey, err := ch.loadWGKey()
	if err != nil && !errors.Is(err, ErrNoWGKey) {
		return fmt.Errorf("loading wg key: %w", err)
	}

	if errors.Is(err, ErrNoWGKey) {
		var keyErr error
		if privateKey, keyErr = wgtypes.GeneratePrivateKey(); keyErr != nil {
			return fmt.Errorf("failed to generate wg keys: %w", keyErr)
		}

		if writeErr := os.WriteFile(ch.wgKeyPath, []byte(privateKey.String()), 0o600); writeErr != nil {
			return fmt.Errorf("writing wg key file: %w", writeErr)
		}
	}

	slog.Info("Fetching config")
	resp, err := ch.ftr.fetchConfig(context.Background(), preferred, privateKey.PublicKey().String())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFetchingConfig, err)
	}
	if resp == nil {
		slog.Info("no new config available")
		return nil
	}
	slog.Info("Config fetched from server")

	// Save the raw config for debugging
	if writeErr := os.WriteFile(strings.TrimSuffix(ch.configPath, ".json")+"_raw.json", resp, 0o600); writeErr != nil {
		slog.Error("writing raw config file", "error", writeErr)
	}

	// Otherwise, we keep the previous config and store any error that might have occurred.
	// We still want to keep the previous config if there was an error. This is important
	// because the error could have been due to temporary network issues, such as brief
	// power loss or internet disconnection.
	// On the other hand, if we have a new config, we want to overwrite any previous error.
	confResp, err := singjson.UnmarshalExtendedContext[C.ConfigResponse](box.BaseContext(), resp)
	if err != nil {
		slog.Error("failed to parse config", "error", err)
		return fmt.Errorf("parsing config: %w", err)
	}
	cleanTags(&confResp)

	if err = setWireGuardKeyInOptions(confResp.Options.Endpoints, privateKey); err != nil {
		slog.Error("failed to replace private key", "error", err)
		return fmt.Errorf("setting wireguard private key: %w", err)
	}
	setCustomProtocolOptions(confResp.Options.Outbounds)
	if err := ch.setConfig(&Config{ConfigResponse: confResp}); err == nil {
		cfg := ch.config.Load().(*Config).ConfigResponse
		locs := make(map[string]C.ServerLocation, len(cfg.OutboundLocations))
		for k, v := range cfg.OutboundLocations {
			if v == nil {
				slog.Warn("Server location is nil, skipping", "tag", k)
				continue
			}
			locs[k] = *v
		}
		opts := servers.Options{
			Outbounds: cfg.Options.Outbounds,
			Endpoints: cfg.Options.Endpoints,
			Locations: locs,
		}
		if err := ch.svrManager.SetServers(servers.SGLantern, opts); err != nil {
			slog.Error("setting servers in manager", "error", err)
		}
	}

	slog.Info("Config fetched")
	return nil
}

func setCustomProtocolOptions(outbounds []option.Outbound) {
	for _, outbound := range outbounds {
		switch opts := outbound.Options.(type) {
		case *lbO.WATEROutboundOptions:
			opts.Dir = filepath.Join(common.DataPath(), "water")
			// TODO: we need to measure the client upload and download metrics
			// in order to set hysteria custom parameters and support brutal sender
			// as congestion control
		default:
		}
	}
}

// TODO: move this to lantern-cloud
func cleanTags(cfg *C.ConfigResponse) {
	opts := cfg.Options
	locs := cfg.OutboundLocations
	nlocs := make(map[string]*C.ServerLocation, len(locs))
	for i := 0; i < len(opts.Outbounds); i++ {
		tag := opts.Outbounds[i].Tag
		loc := locs[tag]
		opts.Outbounds[i].Tag = strings.TrimPrefix(tag, "singbox")
		nlocs[opts.Outbounds[i].Tag] = loc
	}
	for i := 0; i < len(opts.Endpoints); i++ {
		tag := opts.Endpoints[i].Tag
		loc := locs[tag]
		opts.Endpoints[i].Tag = strings.TrimPrefix(tag, "singbox")
		nlocs[opts.Endpoints[i].Tag] = loc
	}
	cfg.OutboundLocations = nlocs
}

func setWireGuardKeyInOptions(endpoints []option.Endpoint, privateKey wgtypes.Key) error {
	// Requires privilege and cannot conflict with existing system interfaces
	// System tries to use system env; for mobile we need to tun device
	system := !(common.IsAndroid() || common.IsIOS() || common.IsMacOS())
	for _, endpoint := range endpoints {
		switch opts := endpoint.Options.(type) {
		case *option.WireGuardEndpointOptions:
			opts.PrivateKey = privateKey.String()
			opts.System = opts.System && system
		case *lbO.AmneziaEndpointOptions:
			opts.PrivateKey = privateKey.String()
			opts.System = opts.System && system
		default:
		}
	}
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
	buf, err := atomicfile.ReadFile(ch.configPath)
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
	emit(nil, cfg)
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
	opts, err := singjson.UnmarshalExtendedContext[C.ConfigResponse](box.BaseContext(), tmp.ConfigResponse)
	if err != nil {
		return nil, err
	}
	return &Config{
		ConfigResponse:    opts,
		PreferredLocation: tmp.PreferredLocation,
	}, nil
}

// saveConfig saves the config to the disk. It creates the config file if it doesn't exist.
func saveConfig(cfg *Config, path string) error {
	// Marshal the config to bytes and write it to the config file.
	// If the config is nil, we don't write anything.
	// This is important because we don't want to overwrite the config file with an empty file.
	buf, err := singjson.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}
	return atomicfile.WriteFile(path, buf, 0644)
}

// GetConfig returns the current configuration. It returns an error if the config is not yet available.
func (ch *ConfigHandler) GetConfig() (*Config, error) {
	cfg := ch.config.Load()
	if cfg == nil {
		return nil, fmt.Errorf("no config yet -- first run?")
	}
	return cfg.(*Config), nil
}

func (ch *ConfigHandler) setConfig(cfg *Config) error {
	slog.Info("Setting config")
	if cfg == nil {
		slog.Warn("Config is nil, not setting")
		return nil
	}
	oldConfig, _ := ch.GetConfig()
	if cfg.PreferredLocation == (C.ServerLocation{}) {
		storedLocation := ch.preferredLocation.Load()
		if storedLocation != nil {
			cfg.PreferredLocation = *storedLocation
		}
	}

	ch.config.Store(cfg)
	slog.Debug("Saving config", "path", ch.configPath)
	if err := saveConfig(cfg, ch.configPath); err != nil {
		slog.Error("saving config", "error", err)
		return fmt.Errorf("saving config: %w", err)
	}
	slog.Info("saved new config")
	slog.Info("Config set")
	emit(oldConfig, cfg)
	return nil
}

// NewConfigEvent is emitted when the configuration changes.
type NewConfigEvent struct {
	events.Event
	Old *Config
	New *Config
}

func emit(old, new *Config) {
	if !reflect.DeepEqual(old, new) {
		events.Emit(NewConfigEvent{Old: old, New: new})
	}
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
	ch.setConfig(cfg)
}
