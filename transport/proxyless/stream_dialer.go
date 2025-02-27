// Package proxyless transport adds a mode in which the client will try proxy-less
// solutions upon its first attempt to connect to an upstream. The client should
// track (in a persistent manner) its previous attempts to use proxy-less solutions
// for each upstream. It should only try connecting to an upstream via proxy-less
// solutions if one of the following is true:
// - This client has never tried proxy-less solutions for this upstream before.
// - This client was able to successfully use a proxy-less solution on its last
// connection to this upstream.
// - This client has received new proxy-less configuration from the back-end since
// its last connection to this upstream.
// - It has been sufficiently long since this client attempted proxy-less solutions
// with this upstream.
package proxyless

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"
	"github.com/getlantern/radiance/config"
)

// TODO: in the future we want to persist this information on a file
// once we have a standard directory for storing radiance info. This should be
// useful for retrieving the last configs used that successfully worked.
type upstreamStatus struct {
	RemoteAddr    string
	NumberOfTries int
	LastResult    bool
	LastSuccess   int64
	ConfigText    string
}

//go:generate mockgen -destination=mock_stream_dialer_test.go -package=proxyless github.com/Jigsaw-Code/outline-sdk/transport StreamDialer

// StreamDialer is a transport.StreamDialer that will try to connect to the upstream by using the proxyless configuration
type StreamDialer struct {
	dialer transport.StreamDialer

	currentConfig            string
	upstreamStatusCache      map[string]upstreamStatus
	upstreamStatusCacheMutex sync.Locker
}

// NewStreamDialer build a Proxyless StreamDialer that will try to connect to the upstream by using the proxyless configuration
// if the conditions are met.
func NewStreamDialer(innerSD transport.StreamDialer, cfg *config.Config) (transport.StreamDialer, error) {
	configText := cfg.GetConnectCfgProxyless().GetConfigText()
	provider := createProvider(innerSD)
	dialer, err := provider.NewStreamDialer(context.Background(), configText)
	if err != nil {
		return nil, fmt.Errorf("failed to created proxyless dialer: %w", err)
	}

	return &StreamDialer{
		dialer:                   dialer,
		upstreamStatusCacheMutex: &sync.Mutex{},
		upstreamStatusCache:      make(map[string]upstreamStatus),
		currentConfig:            configText,
	}, nil
}

// createProvider creates a configurl container provider and register the proxyless techniques
// that we want to use. It also specifies which inner dialer should be used.
func createProvider(innerSD transport.StreamDialer) *configurl.ProviderContainer {
	container := configurl.NewProviderContainer()
	newSD := func(ctx context.Context, config *configurl.Config) (transport.StreamDialer, error) {
		return innerSD, nil
	}
	registerDisorderDialer(&container.StreamDialers, "disorder", newSD)
	registerSplitStreamDialer(&container.StreamDialers, "split", newSD)
	registerTLSFragStreamDialer(&container.StreamDialers, "tlsfrag", newSD)

	return container
}

func (d *StreamDialer) getUpstreamStatus(remoteAddr string) upstreamStatus {
	d.upstreamStatusCacheMutex.Lock()
	defer d.upstreamStatusCacheMutex.Unlock()

	status, ok := d.upstreamStatusCache[remoteAddr]
	if !ok {
		d.upstreamStatusCache[remoteAddr] = upstreamStatus{RemoteAddr: remoteAddr}
		return d.upstreamStatusCache[remoteAddr]
	}
	return status
}

func (d *StreamDialer) updateUpstreamStatus(remoteAddr, configText string, success bool) {
	d.upstreamStatusCacheMutex.Lock()
	defer d.upstreamStatusCacheMutex.Unlock()

	status := d.upstreamStatusCache[remoteAddr]
	status.NumberOfTries++
	status.LastResult = success
	status.ConfigText = configText
	if success {
		status.LastSuccess = time.Now().Unix()
	}
	d.upstreamStatusCache[remoteAddr] = status
}

// DialStream tries to connect to the upstream by using the proxyless configuration if the conditions are met.
// Differently from other DialStream operations, proxyless expects a domain:port instead of ip:port
func (d *StreamDialer) DialStream(ctx context.Context, domain string) (transport.StreamConn, error) {
	status := d.getUpstreamStatus(domain)
	if status.haveNeverTriedProxyless() ||
		status.itWorkedOnLastTry() ||
		status.haveNewConfig(d.currentConfig) ||
		status.lastTryWasLongAgo() {

		conn, err := d.dialer.DialStream(ctx, domain)
		if err != nil {
			d.updateUpstreamStatus(domain, d.currentConfig, false)
			return nil, fmt.Errorf("failed to dial %q via proxyless: %w", domain, err)
		}
		d.updateUpstreamStatus(domain, d.currentConfig, true)
		return conn, nil
	}

	return nil, fmt.Errorf("none conditions met for proxyless request to %q", domain)
}

func (s upstreamStatus) haveNeverTriedProxyless() bool {
	return s.NumberOfTries == 0
}

func (s upstreamStatus) itWorkedOnLastTry() bool {
	return s.LastResult
}

func (s upstreamStatus) haveNewConfig(currentConfig string) bool {
	return s.ConfigText != currentConfig
}

func (s upstreamStatus) lastTryWasLongAgo() bool {
	return time.Unix(s.LastSuccess, 0).Before(time.Now().Add(-48 * time.Hour))
}
