package config

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
)

const configURL = "https://api.iantem.io/v1/config"

// fetcher is responsible for fetching the configuration from the server.
type fetcher struct {
	httpClient *http.Client
}

// newFetcher creates a new fetcher with the given http client.
func newFetcher(client *http.Client) *fetcher {
	return &fetcher{
		httpClient: client,
	}
}

// fetchConfig fetches the configuration from the server. Nil is returned if no new config is available.
func (f *fetcher) fetchConfig() (*ConfigResponse, error) {
	confReq := ConfigRequest{
		ClientInfo: &ConfigRequest_ClientInfo{
			FlashlightVersion: app.Version,
			ClientVersion:     app.ClientVersion,
			UserId:            app.UserId,
			ProToken:          app.ProToken,
			Country:           "",
			Ip:                "",
		},
		Proxy: &ConfigRequest_Proxy{},
	}
	buf, err := proto.Marshal(&confReq)
	if err != nil {
		return nil, fmt.Errorf("marshal config request: %w", err)
	}

	buf, err = f.send(bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if buf == nil { // no new config available
		return nil, nil
	}

	newConf := &ConfigResponse{}
	if err := proto.Unmarshal(buf, newConf); err != nil {
		return nil, fmt.Errorf("unmarshal config response: %w", err)
	}
	return newConf, nil
}

// send sends a request to the server with the given body and returns the response.
func (f *fetcher) send(body io.Reader) ([]byte, error) {
	req, err := backend.NewRequestWithHeaders(context.Background(), http.MethodPost, configURL, body)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}

	buf, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("could not read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d. %s", resp.StatusCode, buf)
	}

	return buf, nil
}
