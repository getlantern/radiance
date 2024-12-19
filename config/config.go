/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	sync "sync"
	"time"

	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"
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
	cfg *Config
	err error
}

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config    eventual.Value
	stopC     chan struct{}
	closeOnce *sync.Once
}

// NewConfigHandler creates a new ConfigHandler that fetches the proxy configuration every pollInterval.
func NewConfigHandler(pollInterval time.Duration) *ConfigHandler {
	ch := &ConfigHandler{
		config:    eventual.NewValue(),
		stopC:     make(chan struct{}),
		closeOnce: &sync.Once{},
	}
	ftr := newFetcher(&http.Client{Timeout: 10 * time.Second})
	go ch.fetchLoop(ftr, pollInterval)
	return ch
}

// fetchLoop fetches the configuration every pollInterval.
func (ch *ConfigHandler) fetchLoop(ftr *fetcher, pollInterval time.Duration) {
	for {
		select {
		case <-ch.stopC:
			return
		case <-time.After(pollInterval):
			proxies, _ := ch.GetConfig(eventual.DontWait)
			resp, err := ftr.fetchConfig()
			if resp != nil {
				// we got a new config and no error so we can update the current config
				proxyList := resp.GetProxy()
				// make sure we have at least one proxy
				if proxyList != nil && len(proxyList.GetProxies()) > 0 {
					proxies = proxyList.GetProxies()[0]
				}
			}
			if err != nil {
				err = fmt.Errorf("%w: %w", ErrFetchingConfig, err)
			}

			// Otherwise, we keep the previous config and store any error that might have occurred.
			// We still want to keep the previous config if there was an error. This is important
			// because the error could have been due to temporary network issues, such as brief
			// power loss or internet disconnection.
			// On the other hand, if we have a new config, we want to overwrite any previous error.
			ch.config.Set(configResult{cfg: proxies, err: err})
		}
	}
}

// GetConfig returns the current proxy configuration. If no configuration is available, GetConfig
// will wait until one is available or the context has expired. If an error occurred during the
// last fetch, that error is returned, as a ErrFetchingConfig, along with the most recent
// configuration, if available. GetConfig is a blocking call.
func (ch *ConfigHandler) GetConfig(ctx context.Context) (*Config, error) {
	_cfgRes, err := ch.config.Get(ctx)
	if err != nil { // ctx expired
		return nil, err
	}
	cfgRes := _cfgRes.(configResult)
	return cfgRes.cfg, cfgRes.err
}

// Stop stops the ConfigHandler from fetching new configurations.
func (ch *ConfigHandler) Stop() {
	ch.closeOnce.Do(func() {
		close(ch.stopC)
	})
}
