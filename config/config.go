package config

import (
	"context"
	"errors"
	"net/http"
	sync "sync"
	"sync/atomic"
	"time"

	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"
)

var (
	log               = golog.LoggerFor("configHandler")
	ErrFetchingConfig = errors.New("still fetching config")
)

// alias for convenience
type Config = ProxyConnectConfig

type ConfigHandler struct {
	config     eventual.Value
	fetcher    *fetcher
	isFetching atomic.Bool
	err        error
	errMu      sync.Mutex
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration
// asynchronously. NewConfigHandler initiates the first fetch.
func NewConfigHandler() *ConfigHandler {
	client := &http.Client{Timeout: 10 * time.Second}
	ch := &ConfigHandler{
		config:  eventual.NewValue(),
		fetcher: newFetcher(client),
	}
	ch.FetchConfig()
	return ch
}

// GetConfig returns the proxy configuration. If no configuration is available, GetConfig waits for
// the next fetch to complete or the context to expire. If an error occurred during the last fetch,
// that error is returned along with the most recent configuration, if available.
//
// GetConfig does not initiate a fetch. See FetchConfig.
func (ch *ConfigHandler) GetConfig(ctx context.Context) (*Config, error) {
	cfg, err := ch.config.Get(ctx)
	if err != nil {
		if ch.isFetching.Load() {
			return nil, ErrFetchingConfig
		}
		return nil, err
	}
	ch.errMu.Lock()
	err = ch.err
	ch.errMu.Unlock()
	return cfg.(*Config), err
}

// FetchConfig fetches the latest proxy configuration asynchronously. If a fetch is already in
// progress, FetchConfig does nothing. It returns true if a fetch was initiated, and false otherwise.
func (ch *ConfigHandler) FetchConfig() bool {
	if !ch.isFetching.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer ch.isFetching.Store(false)
		log.Debug("fetching config")
		ch.setErr(nil)
		cfgres, err := ch.fetcher.fetchConfig()
		if err != nil {
			log.Error(err)
			ch.setErr(err)
			return
		}
		log.Debug("fetched config")
		cfg := filterProxies(cfgres.GetProxy())
		ch.config.Set(cfg)
	}()

	return true
}

func (ch *ConfigHandler) setErr(err error) {
	ch.errMu.Lock()
	ch.err = err
	ch.errMu.Unlock()
}

var supportedProtocols = []string{"shadowsocks"}

// filters proxies for supported protocols, which is currently only shadowsocks
func filterProxies(proxies *ConfigResponse_Proxy) *Config {
	proxyCfgs := proxies.GetProxies()
	if len(proxyCfgs) == 0 {
		return nil
	}

	for _, p := range proxyCfgs {
		if p.GetProtocol() == supportedProtocols[0] {
			return p
		}
	}

	return nil
}
