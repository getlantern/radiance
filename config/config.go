/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

	"github.com/getlantern/radiance/account"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/internal"
)

const (
	maxRetryDelay       = 2 * time.Minute
	defaultPollInterval = 10 * time.Minute
)

var (
	// ErrFetchingConfig is returned by [ConfigHandler.GetConfig] when if there was an error
	// fetching the configuration.
	ErrFetchingConfig = errors.New("failed to fetch config")
)

// Config includes all configuration data from the Lantern API
type Config = C.ConfigResponse

type Options struct {
	PollInterval  time.Duration
	DataPath      string
	Locale        string
	AccountClient *account.Client
	Logger        *slog.Logger
	HTTPClient    *http.Client
}

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config  atomic.Pointer[Config]
	ftr     Fetcher
	logger  *slog.Logger
	options Options

	ctx          context.Context
	cancel       context.CancelFunc
	pollInterval time.Duration
	configPath   string
	wgKeyPath    string
	startOnce    sync.Once
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(ctx context.Context, options Options) *ConfigHandler {
	ctx, cancel := context.WithCancel(ctx)
	pollInterval := options.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	dir := options.DataPath
	ch := &ConfigHandler{
		ctx:          ctx,
		cancel:       cancel,
		pollInterval: pollInterval,
		configPath:   filepath.Join(dir, internal.ConfigFileName),
		wgKeyPath:    filepath.Join(dir, "wg.key"),
		logger:       logger,
		options:      options,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		ch.logger.Error("creating config directory", "error", err)
	}
	if err := ch.loadConfig(); err != nil {
		ch.logger.Error("failed to load config", "error", err)
	}
	return ch
}

func (ch *ConfigHandler) Start() {
	ch.startOnce.Do(func() {
		ch.ftr = newFetcher(ch.options.Locale, ch.options.AccountClient, ch.options.HTTPClient)
		go ch.fetchLoop(ch.pollInterval)
		events.Subscribe(func(evt account.UserChangeEvent) {
			ch.logger.Debug("User change detected that requires config refetch")
			if err := ch.fetchConfig(); err != nil {
				ch.logger.Error("Failed to fetch config", "error", err)
			}
		})
	})
}

var ErrNoWGKey = errors.New("no wg key")

func (ch *ConfigHandler) loadWGKey() (wgtypes.Key, error) {
	buf, err := atomicfile.ReadFile(ch.wgKeyPath)
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

func (ch *ConfigHandler) fetchConfig() error {
	if settings.GetBool(settings.ConfigFetchDisabledKey) {
		ch.logger.Info("config fetch disabled, skipping")
		return nil
	}
	if ch.isClosed() {
		return fmt.Errorf("config handler is closed")
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

		if writeErr := atomicfile.WriteFile(ch.wgKeyPath, []byte(privateKey.String()), 0o600); writeErr != nil {
			return fmt.Errorf("writing wg key file: %w", writeErr)
		}
	}

	ch.logger.Info("Fetching config")
	preferred := common.PreferredLocation{}
	if err := settings.GetStruct(settings.PreferredLocationKey, &preferred); err != nil {
		ch.logger.Error("failed to get preferred location from settings", "error", err)
	}

	resp, err := ch.ftr.fetchConfig(ch.ctx, preferred, privateKey.PublicKey().String())
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFetchingConfig, err)
	}
	if resp == nil {
		ch.logger.Info("no new config available")
		return nil
	}
	ch.logger.Info("Config fetched from server")

	// Save the raw config for debugging
	if writeErr := atomicfile.WriteFile(strings.TrimSuffix(ch.configPath, ".json")+"_raw.json", resp, 0o600); writeErr != nil {
		ch.logger.Error("writing raw config file", "error", writeErr)
	}

	// Otherwise, we keep the previous config and store any error that might have occurred.
	// We still want to keep the previous config if there was an error. This is important
	// because the error could have been due to temporary network issues, such as brief
	// power loss or internet disconnection.
	// On the other hand, if we have a new config, we want to overwrite any previous error.
	confResp, err := singjson.UnmarshalExtendedContext[C.ConfigResponse](box.BaseContext(), resp)
	if err != nil {
		ch.logger.Error("failed to parse config", "error", err)
		return fmt.Errorf("parsing config: %w", err)
	}
	cleanTags(&confResp)

	setWireGuardKeyInOptions(confResp.Options.Endpoints, privateKey)
	setCustomProtocolOptions(confResp.Options.Outbounds)
	if err := ch.setConfig(&confResp); err != nil {
		ch.logger.Error("failed to set config", "error", err)
		return fmt.Errorf("setting config: %w", err)
	}
	ch.logger.Info("Config fetched")
	return nil
}

func setCustomProtocolOptions(outbounds []option.Outbound) {
	for _, outbound := range outbounds {
		switch opts := outbound.Options.(type) {
		case *lbO.WATEROutboundOptions:
			opts.Dir = filepath.Join(settings.GetString(settings.DataPathKey), "water")
			// TODO: we need to measure the client upload and download metrics
			// in order to set hysteria custom parameters and support brutal sender
			// as congestion control
		default:
		}
	}
}

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

func setWireGuardKeyInOptions(endpoints []option.Endpoint, privateKey wgtypes.Key) {
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
}

// fetchLoop fetches the configuration periodically. It uses the server's
// recommended poll interval (PollIntervalSeconds) when available, falling
// back to the default pollInterval. This allows the bandit to control how
// often the client re-fetches based on learning confidence.
func (ch *ConfigHandler) fetchLoop(defaultPollInterval time.Duration) {
	backoff := common.NewBackoff(maxRetryDelay)
	for {
		if err := ch.fetchConfig(); err != nil {
			ch.logger.Error("Failed to fetch config. Retrying", "error", err)
			backoff.Wait(ch.ctx)
			if ch.ctx.Err() != nil {
				return
			}
			continue
		}
		backoff.Reset()

		// Use server-recommended poll interval if available, clamped to a
		// minimum of 10s to prevent excessive polling.
		interval := defaultPollInterval
		if cfg := ch.config.Load(); cfg != nil && cfg.PollIntervalSeconds > 0 {
			serverInterval := time.Duration(cfg.PollIntervalSeconds) * time.Second
			if serverInterval < 10*time.Second {
				serverInterval = 10 * time.Second
			}
			interval = serverInterval
			ch.logger.Debug("Using server-recommended poll interval",
				"interval", interval,
				"default", defaultPollInterval,
			)
		}

		select {
		case <-ch.ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Stop stops the ConfigHandler from fetching new configurations.
func (ch *ConfigHandler) Stop() {
	ch.cancel()
}

func (ch *ConfigHandler) isClosed() bool {
	select {
	case <-ch.ctx.Done():
		return true
	default:
		return false
	}
}

// loadConfig loads the config file from the disk. If the config file is not found, it returns
// nil.
func (ch *ConfigHandler) loadConfig() error {
	ch.logger.Debug("reading config file")
	cfg, err := load(ch.configPath)
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}
	if cfg != nil {
		ch.config.Store(cfg)
		emit(nil, cfg)
	}
	return nil
}

func load(path string) (*Config, error) {
	buf, err := atomicfile.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // No config file yet
	}
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	ctx := box.BaseContext()
	cfg, err := singjson.UnmarshalExtendedContext[*Config](ctx, buf)
	if err != nil {
		// try to migrate from old format if parsing fails
		// TODO(3/06, garmr-ulfr): remove this migration code after a few releases
		if cfg, err = migrateToNewFmt(buf); err == nil {
			saveConfig(cfg, path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return cfg, nil
}

func migrateToNewFmt(data []byte) (*Config, error) {
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
	settings.Set(settings.PreferredLocationKey, &tmp.PreferredLocation)
	return &opts, nil
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
	return cfg, nil
}

func (ch *ConfigHandler) setConfig(cfg *Config) error {
	ch.logger.Info("Setting config")
	if cfg == nil {
		ch.logger.Warn("Config is nil, not setting")
		return nil
	}
	oldConfig, _ := ch.GetConfig()
	ch.config.Store(cfg)
	ch.logger.Debug("Saving config", "path", ch.configPath)
	if err := saveConfig(cfg, ch.configPath); err != nil {
		ch.logger.Error("saving config", "error", err)
		return fmt.Errorf("saving config: %w", err)
	}
	ch.logger.Info("saved new config")
	ch.logger.Info("Config set")
	if !ch.isClosed() {
		emit(oldConfig, cfg)
	}
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
