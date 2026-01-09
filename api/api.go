package api

import (
	"log/slog"
	"path/filepath"
	"strconv"

	"github.com/go-resty/resty/v2"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/kindling"
)

const tracerName = "github.com/getlantern/radiance/api"

type APIClient struct {
	salt       []byte
	saltPath   string
	userData   *protos.LoginResponse
	deviceID   string
	authClient AuthClient
	userInfo   common.UserInfo
}

func NewAPIClient(userInfo common.UserInfo, dataDir string) *APIClient {
	userData, err := userInfo.GetData()
	if err != nil {
		slog.Warn("failed to get user data", "error", err)
	}
	path := filepath.Join(dataDir, saltFileName)
	salt, err := readSalt(path)
	if err != nil {
		slog.Warn("failed to read salt", "error", err)
	}

	cli := &APIClient{
		salt:       salt,
		saltPath:   path,
		userData:   userData,
		deviceID:   userInfo.DeviceID(),
		authClient: &authClient{userInfo},
		userInfo:   userInfo,
	}
	return cli
}

func (a *APIClient) proWebClient() *webClient {
	httpClient := kindling.HTTPClient()
	proWC := newWebClient(httpClient, proServerURL)
	proWC.client.OnBeforeRequest(func(client *resty.Client, req *resty.Request) error {
		req.Header.Set(backend.DeviceIDHeader, a.userInfo.DeviceID())
		if a.userInfo.LegacyToken() != "" {
			req.Header.Set(backend.ProTokenHeader, a.userInfo.LegacyToken())
		}
		if a.userInfo.LegacyID() != 0 {
			req.Header.Set(backend.UserIDHeader, strconv.FormatInt(a.userInfo.LegacyID(), 10))
		}
		return nil
	})
	return proWC
}

func authWebClient() *webClient {
	return newWebClient(kindling.HTTPClient(), baseURL)
}
