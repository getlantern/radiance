package api

import (
	"log/slog"
	"net/http"
	"path/filepath"

	"github.com/getlantern/radiance/api/protos"
	"github.com/getlantern/radiance/common"
)

type APIClient struct {
	wc    *webClient
	proWC *webClient

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
	wc := newWebClient(httpClient, baseURL, userInfo)
	return &APIClient{
		wc:         wc,
		proWC:      proWC,
		salt:       salt,
		saltPath:   path,
		userData:   userData,
		deviceID:   userInfo.DeviceID(),
		authClient: &authClient{wc},
		userInfo:   userInfo,
	}
}
