package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/kindling"
)

const tracerName = "github.com/getlantern/radiance/api"

type APIClient struct {
	salt       []byte
	saltPath   string
	authClient AuthClient
	mu         sync.RWMutex
}

func NewAPIClient(dataDir string) *APIClient {
	path := filepath.Join(dataDir, saltFileName)
	salt, err := readSalt(path)
	if err != nil {
		slog.Warn("failed to read salt", "error", err)
	}

	cli := &APIClient{
		salt:       salt,
		saltPath:   path,
		authClient: &authClient{},
	}
	return cli
}

func (a *APIClient) proWebClient() *webClient {
	httpClient := kindling.HTTPClient()
	proWC := newWebClient(httpClient, common.GetProServerURL())
	proWC.client.OnBeforeRequest(func(client *resty.Client, req *resty.Request) error {
		req.Header.Set(backend.DeviceIDHeader, settings.GetString(settings.DeviceIDKey))
		if settings.GetString(settings.TokenKey) != "" {
			req.Header.Set(backend.ProTokenHeader, settings.GetString(settings.TokenKey))
		}
		if settings.GetInt64(settings.UserIDKey) != 0 {
			req.Header.Set(backend.UserIDHeader, strconv.FormatInt(settings.GetInt64(settings.UserIDKey), 10))
		}
		return nil
	})
	return proWC
}

func authWebClient() *webClient {
	return newWebClient(kindling.HTTPClient(), common.GetBaseURL())
}

// tunnelClient routes through the local tunnel proxy when the VPN is running.
// Unlike the bypass proxy (which routes to direct), this routes through the
// active VPN proxy outbound. No client-level timeout — SSE streams are
// long-lived. When the VPN is not running, connections fail (no fallback).
var tunnelClient = &http.Client{
	Transport: &http.Transport{
		DialContext:           bypass.TunnelDialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}
