package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/go-resty/resty/v2"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common/settings"
)

const tracerName = "github.com/getlantern/radiance/api"

type APIClient struct {
	authWc     *webClient
	proWC      *webClient
	salt       []byte
	saltPath   string
	authClient AuthClient
	mu         sync.RWMutex
}

func NewAPIClient(httpClient *http.Client, dataDir string) *APIClient {
	path := filepath.Join(dataDir, saltFileName)
	salt, err := readSalt(path)
	if err != nil {
		slog.Warn("failed to read salt", "error", err)
	}

	proWC := newWebClient(httpClient, proServerURL)
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
	wc := newWebClient(httpClient, baseURL)
	return &APIClient{
		authWc:     wc,
		proWC:      proWC,
		salt:       salt,
		saltPath:   path,
		authClient: &authClient{wc},
	}
}
