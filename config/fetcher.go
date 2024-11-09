package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/proto"
)

const (
	configURL = "https://api.iantem.io/v1/config"

	appVersionHeader = "X-Lantern-App-Version"
	versionHeader    = "X-Lantern-Version"
	platformHeader   = "X-Lantern-Platform"
	appNameHeader    = "X-Lantern-App"
	deviceIdHeader   = "X-Lantern-Device-Id"
)

var (
	clientVersion = "7.6.47"
	version       = "7.6.47"
	userID        = "2089345"
	proToken      = ""
)

type fetcher struct {
	httpClient *http.Client
}

func newFetcher(client *http.Client) *fetcher {
	return &fetcher{
		httpClient: client,
	}
}

func (f *fetcher) fetchConfig() (*ConfigResponse, error) {
	confReq := ConfigRequest{
		ClientInfo: &ConfigRequest_ClientInfo{
			FlashlightVersion: version,
			ClientVersion:     clientVersion,
			UserId:            userID,
			ProToken:          proToken,
			Country:           "",
			Ip:                "",
		},
		Proxy: &ConfigRequest_Proxy{},
	}
	buf, err := proto.Marshal(&confReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal config request: %w", err)
	}

	buf, statusCode, err := f.fetch(bytes.NewReader(buf))
	if err != nil {
		if statusCode == 0 {
			return nil, fmt.Errorf("failed to fetch config: %w", err)
		}
		return nil, fmt.Errorf("failed to fetch config: %d %w", statusCode, err)
	}

	newConf := &ConfigResponse{}
	if err := proto.Unmarshal(buf, newConf); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config response: %w", err)
	}
	return newConf, nil
}

func (f *fetcher) fetch(b io.Reader) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodPost, configURL, b)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create config request: %w", err)
	}
	addHeaders(req)
	resp, err := f.httpClient.Do(req)
	if err != nil {
		if resp != nil {
			return nil, resp.StatusCode, errors.New(resp.Status)
		}
		return nil, 0, err
	}

	buf, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, 0, fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, resp.StatusCode, errors.New(string(buf))
	}

	return buf, resp.StatusCode, nil
}

func addHeaders(req *http.Request) {
	req.Header.Set(appVersionHeader, clientVersion)
	req.Header.Set(versionHeader, version) // panics if not set
	req.Header.Set(platformHeader, "linux")
	req.Header.Set(appNameHeader, "radiance")
	req.Header.Set(deviceIdHeader, "some-uuid-here")

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Cache-Control", "no-cache")
}
