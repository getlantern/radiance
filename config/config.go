/*
Package config provides a handler for fetching and storing proxy configurations.
*/
package config

import (
	"context"
	"errors"
	"fmt"
	sync "sync"
	"time"

	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"
	"github.com/getlantern/kindling"
	"github.com/getlantern/radiance/backend/apipb"
	"github.com/getlantern/radiance/common/reporting"
)

var (
	log = golog.LoggerFor("config")

	// ErrFetchingConfig is returned by [ConfigHandler.GetConfig] when if there was an error
	// fetching the configuration.
	ErrFetchingConfig = errors.New("failed to fetch config")
)

// alias for convenience
type Config = apipb.ProxyConnectConfig

type configResult struct {
	cfg []*Config
	err error
}

// ConfigHandler handles fetching the proxy configuration from the proxy server. It provides access
// to the most recent configuration.
type ConfigHandler struct {
	// config holds a configResult.
	config    eventual.Value
	ftr       *fetcher
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
	// TODO: Ideally we would know the user locale here on radiance startup.
	k := kindling.NewKindling(
		kindling.WithPanicListener(reporting.PanicListener),
		kindling.WithDomainFronting("https://raw.githubusercontent.com/getlantern/lantern-binaries/refs/heads/main/fronted.yaml.gz", ""),
		kindling.WithProxyless("api.iantem.io"),
	)
	ch.ftr = newFetcher(k.NewHTTPClient())
	go ch.fetchLoop(pollInterval)
	return ch
}

func (ch *ConfigHandler) fetchConfig() error {
	log.Debug("Fetching config")
	proxies, _ := ch.GetConfig(eventual.DontWait)
	resp, err := ch.ftr.fetchConfig()
	if resp != nil {
		log.Debug("received config response")
		// we got a new config and no error so we can update the current config
		proxyList := resp.GetProxy()
		// make sure we have at least one proxy
		if proxyList != nil && len(proxyList.GetProxies()) > 0 {
			log.Debugf("received %d new proxies", len(proxyList.GetProxies()))
			proxies = proxyList.GetProxies()
			log.Debugf("new proxy: %+v", proxies)
		} else {
			log.Debug("proxy list is empty")
		}
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrFetchingConfig, err)
	}

	// Otherwise, we keep the previous config and store any error that might have occurred.
	// We still want to keep the previous config if there was an error. This is important
	// because the error could have been due to temporary network issues, such as brief
	// power loss or internet disconnection.
	// On the other hand, if we have a new config, we want to overwrite any previous error.
	ch.config.Set(configResult{cfg: proxies, err: err})

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

// GetConfig returns the current proxy configuration. If no configuration is available, GetConfig
// will wait until one is available or the context has expired. If an error occurred during the
// last fetch, that error is returned, as a ErrFetchingConfig, along with the most recent
// configuration, if available. GetConfig is a blocking call.
func (ch *ConfigHandler) GetConfig(ctx context.Context) ([]*Config, error) {
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
