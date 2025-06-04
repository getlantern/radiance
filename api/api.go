package api

import (
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/go-resty/resty/v2"
)

type APIClient struct {
	authWc *webClient
	proWC  *webClient

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

	proWC := newWebClient(httpClient, proServerURL, userInfo)
	proWC.client.SetPreRequestHook(func(client *resty.Client, req *http.Request) error {
		req.Header.Set(backend.DeviceIDHeader, userInfo.DeviceID())
		if userInfo.LegacyToken() != "" {
			req.Header.Set(backend.ProTokenHeader, userInfo.LegacyToken())
		}
		if userInfo.LegacyID() != 0 {
			req.Header.Set(backend.UserIDHeader, strconv.FormatInt(userInfo.LegacyID(), 10))
		}
		return nil
	})
	wc := newWebClient(httpClient, baseURL, userInfo)
	wc.client.SetPreRequestHook(func(client *resty.Client, req *http.Request) error {
		if req.URL != nil && strings.HasSuffix(req.URL.Path, "/users/signup") {
			req.Header.Set(backend.DeviceIDHeader, userInfo.DeviceID())
			if userInfo.LegacyToken() != "" {
				req.Header.Set(backend.ProTokenHeader, userInfo.LegacyToken())
			}
			if userInfo.LegacyID() != 0 {
				req.Header.Set(backend.UserIDHeader, strconv.FormatInt(userInfo.LegacyID(), 10))
			}
		}
		return nil
	})

	return &APIClient{
		authWc:     wc,
		proWC:      proWC,
		salt:       salt,
		saltPath:   path,
		userData:   userData,
		deviceID:   userInfo.DeviceID(),
		authClient: &authClient{wc},
		userInfo:   userInfo,
	}
}
