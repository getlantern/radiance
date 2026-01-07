package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/go-resty/resty/v2"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
)

const tracerName = "github.com/getlantern/radiance/api"

type APIClient struct {
	authWcFunc func() *webClient
	proWCFunc  func() *webClient

	salt       []byte
	saltPath   string
	userData   *protos.LoginResponse
	deviceID   string
	authClient AuthClient
	userInfo   common.UserInfo
}

func NewAPIClient(httpClientFunc func() *http.Client, userInfo common.UserInfo, dataDir string) *APIClient {
	userData, err := userInfo.GetData()
	if err != nil {
		slog.Warn("failed to get user data", "error", err)
	}
	path := filepath.Join(dataDir, saltFileName)
	salt, err := readSalt(path)
	if err != nil {
		slog.Warn("failed to read salt", "error", err)
	}
	authWcFunc := func() *webClient {
		return newWebClient(httpClientFunc, "")
	}
	return &APIClient{
		authWcFunc: authWcFunc,
		proWCFunc: func() *webClient {
			proWC := newWebClient(httpClientFunc, proServerURL)
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
		},
		salt:       salt,
		saltPath:   path,
		userData:   userData,
		deviceID:   userInfo.DeviceID(),
		authClient: &authClient{authWcFunc, userInfo},
		userInfo:   userInfo,
	}
}
