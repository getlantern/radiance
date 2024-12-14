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
	"sync/atomic"
	"time"

	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"
)

var (
	log = golog.LoggerFor("config")

	// ErrFetchingConfig is returned by [ConfigHandler.GetConfig] when the ctx expires before a
	// proxy config is available and a fetch is in progress.
	ErrFetchingConfig = errors.New("fetch in progress")
)

// alias for convenience
type Config = ProxyConnectConfig

// ConfigHandler fetches and stores the proxy configuration asynchronously. It provides access to
// the most recent configuration.
type ConfigHandler struct {
	// config is an eventual.Value that holds a [Config] object.
	config     eventual.Value
	fetcher    *fetcher
	isFetching atomic.Bool

	// err is the error that occurred during the last fetch. It is set to nil if the last fetch was
	// successful.
	err   error
	errMu sync.Mutex
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

// GetConfig returns the current proxy configuration. If no configuration is available, GetConfig
// waits for the next fetch to complete or the context to expire. If an error occurred during the
// last fetch, that error is returned along with the most recent configuration, if available.
//
// GetConfig does not initiate a fetch. See [ConfigHandler.FetchConfig].
func (ch *ConfigHandler) GetConfig(ctx context.Context) (*Config, error) {
	cfg, err := ch.config.Get(ctx)
	if err != nil { // no config available
		if ch.isFetching.Load() {
			return nil, fmt.Errorf("config unavailable: %w", ErrFetchingConfig)
		}
		if ferr := ch.getFErr(); ferr != nil {
			// fetch failed and has not been retried yet
			return nil, ferr
		}

		// fetch only returns a nil config if 1) the config we have currently doesn't need to be
		// updated or 2) we encountered an error while fetching. If we don't have a config yet, we
		// should be either fetching or have an error. We should never reach here. If we do, we have
		// a bigger problem; the server is not assigning us proxies and is returning '204 no content'.
		//
		// Or someone modified the code and didn't test it properly..
		return nil, errors.New("no config received from server")
	}
	return cfg.(*Config), ch.getFErr()
}

// FetchConfig initiates a fetch for a proxy config. The fetch is performed asynchronously. If a fetch
// is already in progress, FetchConfig does nothing. It returns true if a fetch was initiated, and
// false otherwise.
func (ch *ConfigHandler) FetchConfig() bool {
	if !ch.isFetching.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer ch.isFetching.Store(false)
		log.Debug("fetching config")
		ch.setFErr(nil)
		cfgres, err := ch.fetcher.fetchConfig()
		if err != nil {
			log.Error(err)
			ch.setFErr(err)
			return
		}

		if cfgres == nil {
			log.Debug("already have the most recent config")
			return
		}

		pconf := cfgres.GetProxy()
		if pconf == nil || len(pconf.GetProxies()) == 0 {
			log.Debugf("received config with no proxies: %v", cfgres)
			return
		}
		log.Debugf("received config: %v", cfgres)
		ch.config.Set(pconf.GetProxies()[0])
	}()

	return true
}

func (ch *ConfigHandler) setFErr(err error) {
	ch.errMu.Lock()
	ch.err = err
	ch.errMu.Unlock()
}

func (ch *ConfigHandler) getFErr() error {
	ch.errMu.Lock()
	err := ch.err
	ch.errMu.Unlock()
	return err
}
