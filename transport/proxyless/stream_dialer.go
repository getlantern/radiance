// proxyless transport adds a mode in which the client will try proxy-less solutions upon its first attempt to connect to an upstream. The client should track (in a persistent manner) its previous attempts to use proxy-less solutions for each upstream. It should only try connecting to an upstream via proxy-less solutions if one of the following is true:
// - This client has never tried proxy-less solutions for this upstream before.
// - This client was able to successfully use a proxy-less solution on its last connection to this upstream.
// - This client has received new proxy-less configuration from the back-end since its last connection to this upstream.
// - It has been sufficiently long since this client attempted proxy-less solutions with this upstream. Let's initially set this to 48 hours.
package proxyless

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/x/configurl"
	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/config"
)

var log = golog.LoggerFor("transport.proxyless")

type upstreamStatus struct {
	RemoteAddr    string
	NumberOfTries int
	LastResult    bool
	LastSuccess   int64
	ConfigText    string
}

//go:generate mockgen -destination=mock_stream_dialer_test.go -package=proxyless github.com/Jigsaw-Code/outline-sdk/transport StreamDialer

// StreamDialer is a wrapper around a StreamDialer that is used for debugging. Currently, it logs
// the data written to and read from the connection. This will be removed in the future.
type StreamDialer struct {
	innerSD         transport.StreamDialer
	proxylessDialer transport.StreamDialer

	currentConfig            string
	upstreamStatusCache      map[string]upstreamStatus
	upstreamStatusCacheMutex sync.Locker
}

func NewStreamDialer(innerSD transport.StreamDialer, cfg *config.Config) (transport.StreamDialer, error) {
	if innerSD == nil {
		return nil, errors.New("dialer must not be nil")
	}

	provider := configurl.NewDefaultProviders()
	configText := cfg.GetConnectCfgProxyless().GetConfigText()
	dialer, err := provider.NewStreamDialer(context.Background(), configText)
	if err != nil {
		return nil, fmt.Errorf("failed to created proxyless dialer: %w", err)
	}

	return &StreamDialer{
		innerSD:                  innerSD,
		proxylessDialer:          dialer,
		upstreamStatusCacheMutex: &sync.Mutex{},
		upstreamStatusCache:      make(map[string]upstreamStatus),
		currentConfig:            configText,
	}, nil
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

func (d *StreamDialer) DialStream(ctx context.Context, remoteAddr string) (transport.StreamConn, error) {
	status := d.getUpstreamStatus(remoteAddr)
	if status.haveNeverTriedProxyless() ||
		status.itWorkedOnLastTry() ||
		status.haveNewConfig(d.currentConfig) ||
		status.lastTryWasLongAgo() {
		conn, err := d.proxylessDialer.DialStream(ctx, remoteAddr)
		if err == nil {
			d.updateUpstreamStatus(remoteAddr, d.currentConfig, true)
			return conn, nil
		}
		d.updateUpstreamStatus(remoteAddr, d.currentConfig, false)
		log.Errorf("failed to dial %s via proxyless: %v", remoteAddr, err)
	}

	return d.innerSD.DialStream(ctx, remoteAddr)
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
