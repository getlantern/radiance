package api

import (
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/go-resty/resty/v2"

	"github.com/getlantern/radiance/backend"
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
	proWC := newWebClient(httpClient, stageProServerURL)
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
	return newWebClient(kindling.HTTPClient(), stageBaseURL)
}
