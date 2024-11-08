package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/proto"
)

const configURL = "https://api.iantem.io/v1/config"

var (
	clientVersion = "9999.99.99-dev"
	version       = "9999.99.99"
	userID        = "12343"
	proToken      = ""
)

type fetcher struct {
	httpClient *http.Client
}

func newFetcher() *fetcher {
	return &fetcher{
		httpClient: &http.Client{},
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
		return nil, 0, fmt.Errorf("failed to send request: %w", err)
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
	req.Header.Set("X-Lantern-App-Version", clientVersion)
	req.Header.Set("X-Lantern-Version", version) // panics if not set
	req.Header.Set("X-Lantern-Platform", "linux")
	req.Header.Set("X-Lantern-App", "radiance")
	req.Header.Set("X-Lantern-Device-Id", "some-uuid-here")

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Cache-Control", "no-cache")
}
