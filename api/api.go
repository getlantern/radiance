package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/go-resty/resty/v2"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/kindling"
)

const tracerName = "github.com/getlantern/radiance/api"

type APIClient struct {
	authWc          *webClient
	proWC           *webClient
	httpClientMutex sync.Mutex

	salt       []byte
	saltPath   string
	userData   *protos.LoginResponse
	deviceID   string
	authClient AuthClient
	userInfo   common.UserInfo
}

func NewAPIClient(httpClient *http.Client, userInfo common.UserInfo, dataDir string) *APIClient {
	userData, err := userInfo.GetData()
	if err != nil {
		slog.Warn("failed to get user data", "error", err)
	}
	path := filepath.Join(dataDir, saltFileName)
	salt, err := readSalt(path)
	if err != nil {
		slog.Warn("failed to read salt", "error", err)
	}

	wc := newWebClient(httpClient, baseURL)
	cli := &APIClient{
		authWc:     wc,
		proWC:      buildProWebClient(httpClient, proServerURL, userInfo),
		salt:       salt,
		saltPath:   path,
		userData:   userData,
		deviceID:   userInfo.DeviceID(),
		authClient: &authClient{wc, userInfo},
		userInfo:   userInfo,
	}
	events.Subscribe(func(kindling.ClientUpdated) {
		cli.httpClientMutex.Lock()
		defer cli.httpClientMutex.Unlock()

		newHTTPClient := kindling.HTTPClient()
		cli.proWC = buildProWebClient(newHTTPClient, proServerURL, userInfo)
		cli.authWc = newWebClient(httpClient, baseURL)
		cli.authClient = &authClient{cli.authWc, userInfo}
	})
	return cli
}

func buildProWebClient(httpClient *http.Client, proServerURL string, userInfo common.UserInfo) *webClient {
	proWC := newWebClient(httpClient, proServerURL)
	proWC.client.OnBeforeRequest(func(client *resty.Client, req *resty.Request) error {
		req.Header.Set(backend.DeviceIDHeader, userInfo.DeviceID())
		if userInfo.LegacyToken() != "" {
			req.Header.Set(backend.ProTokenHeader, userInfo.LegacyToken())
		}
		if userInfo.LegacyID() != 0 {
			req.Header.Set(backend.UserIDHeader, strconv.FormatInt(userInfo.LegacyID(), 10))
		}
		return nil
	})
	return proWC
}
