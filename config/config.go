/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	sync "sync"
	"time"

	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/user"
	"google.golang.org/protobuf/encoding/protojson"
)

const (
	configDir      = "config"
	configFileName = "proxy.conf"
)

var (
	log = golog.LoggerFor("config")

	// ErrFetchingConfig is returned by [ConfigHandler.GetConfig] when if there was an error
	// fetching the configuration.
	ErrFetchingConfig = errors.New("failed to fetch config")
)

// alias for convenience
type Config = ProxyConnectConfig

type configResult struct {
	cfg     []*Config
	country string
	err     error
}

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config    eventual.Value
	ftr       *fetcher
	apiClient common.WebClient
	stopC     chan struct{}
	closeOnce *sync.Once

	configPath string
}

type PreferredServerLocation struct {
	Country string
	City    string
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(pollInterval time.Duration, httpClient *http.Client, user *user.User, preferredServerLocation *PreferredServerLocation) *ConfigHandler {
	ch := &ConfigHandler{
		config:     eventual.NewValue(),
		stopC:      make(chan struct{}),
		closeOnce:  &sync.Once{},
		configPath: filepath.Join(configDir, configFileName),
		apiClient:  common.NewWebClient(httpClient),
	}
	// if err := ch.loadConfig(); err != nil {
	// 	log.Errorf("failed to load config: %v", err)
	// }

	ch.ftr = newFetcher(httpClient, user, preferredServerLocation)
	go ch.fetchLoop(pollInterval)
	return ch
}

func (ch *ConfigHandler) ListAvailableServers(ctx context.Context) ([]*ListAvailableResponse_AvailableRegion, error) {
	var resp ListAvailableResponse
	if err := ch.apiClient.GetPROTOC(ctx, "/available-servers", nil, &resp); err != nil {
		return nil, err
	}

	return resp.GetRegions(), nil
}

func (ch *ConfigHandler) fetchConfig() error {
	log.Debug("Fetching config")
	proxies, country, _ := ch.GetConfig(eventual.DontWait)
	resp, err := ch.ftr.fetchConfig()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFetchingConfig, err)
	}

	if resp != nil {
		log.Debug("received config response")
		country = resp.GetCountry()
		// we got a new config and no error so we can update the current config
		proxyList := resp.GetProxy()
		// make sure we have at least one proxy
		if proxyList != nil && len(proxyList.GetProxies()) > 0 {
			log.Debugf("received %d new proxies", len(proxyList.GetProxies()))
			proxies = proxyList.GetProxies()
			log.Debugf("new proxy: %+v", proxies)
			if sErr := saveConfig(ch.configPath, proxies[0]); sErr != nil {
				log.Errorf("failed to save config: %v", sErr)
			}
		} else {
			log.Debug("proxy list is empty")
		}
	}

	// Otherwise, we keep the previous config and store any error that might have occurred.
	// We still want to keep the previous config if there was an error. This is important
	// because the error could have been due to temporary network issues, such as brief
	// power loss or internet disconnection.
	// On the other hand, if we have a new config, we want to overwrite any previous error.
	ch.config.Set(configResult{cfg: proxies, country: country, err: err})

	return nil
}

// fetchLoop fetches the configuration every pollInterval.
func (ch *ConfigHandler) fetchLoop(pollInterval time.Duration) {
	if err := ch.fetchConfig(); err != nil {
		log.Errorf("Failed to fetch config: %v. Retrying in %v", err, pollInterval)
	}
	for {
		select {
		case <-ch.stopC:
			return
		case <-time.After(pollInterval):
			if err := ch.fetchConfig(); err != nil {
				log.Errorf("Failed to fetch config: %v. Retrying in %v", err, pollInterval)
			}
		}
	}
}

// GetConfig returns the current proxy configuration and the country. If no configuration is available, GetConfig
// will wait until one is available or the context has expired. If an error occurred during the
// last fetch, that error is returned, as a ErrFetchingConfig, along with the most recent
// configuration, if available. GetConfig is a blocking call.
func (ch *ConfigHandler) GetConfig(ctx context.Context) ([]*Config, string, error) {
	_cfgRes, err := ch.config.Get(ctx)
	if err != nil { // ctx expired
		return nil, "", err
	}
	cfgRes := _cfgRes.(configResult)
	return cfgRes.cfg, cfgRes.country, cfgRes.err
}

// Stop stops the ConfigHandler from fetching new configurations.
func (ch *ConfigHandler) Stop() {
	ch.closeOnce.Do(func() {
		close(ch.stopC)
	})
}

// loadConfig loads the configuration from the disk and sets it in the ConfigHandler.
func (ch *ConfigHandler) loadConfig() error {
	log.Debug("Loading config")
	cfg, err := loadConfig(ch.configPath)
	if err != nil {
		err = fmt.Errorf("loading config: %w", err)
		log.Error(err)
		return err
	}
	log.Debug("Config loaded")
	if cfg == nil { // no config file
		log.Debug("No config file found")
		return nil
	}
	log.Debug("Setting config")
	ch.config.Set(configResult{cfg: []*Config{cfg}})
	return nil
}

// loadConfig loads the config file from the disk. If the config file is not found, it returns
// nil.
func loadConfig(path string) (*Config, error) {
	log.Debugf("reading config file at %s", path)
	buf, err := os.ReadFile(path)
	log.Debug("config file read")
	if os.IsNotExist(err) { // no config file
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var cfg Config
	err = protojson.Unmarshal(buf, &cfg)
	if err != nil {
		return nil, fmt.Errorf("unmarshaling config: %w", err)
	}
	return &cfg, nil
}

// saveConfig saves the configuration to the disk.
func saveConfig(path string, cfg *Config) error {
	buf, err := protojson.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, buf, 0644)
}
