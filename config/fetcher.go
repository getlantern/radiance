package config

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strconv"

	"google.golang.org/protobuf/proto"

	"slices"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/user"
)

const configURL = "https://api.iantem.io/v1/config"

// fetcher is responsible for fetching the configuration from the server.
type fetcher struct {
	httpClient              *http.Client
	user                    *user.User
	preferredServerLocation *PreferredServerLocation
}

// newFetcher creates a new fetcher with the given http client.
func newFetcher(client *http.Client, user *user.User, preferredServerLocation *PreferredServerLocation) *fetcher {
	return &fetcher{
		httpClient:              client,
		user:                    user,
		preferredServerLocation: preferredServerLocation,
	}
}

// fetchConfig fetches the configuration from the server. Nil is returned if no new config is available.
func (f *fetcher) fetchConfig() (*ConfigResponse, error) {
	var preferredRegion *ConfigRequest_PreferredRegion
	if f.preferredServerLocation != nil {
		preferredRegion = &ConfigRequest_PreferredRegion{
			Country: f.preferredServerLocation.Country,
			City:    f.preferredServerLocation.City,
		}
	}
	confReq := ConfigRequest{
		ClientInfo: &ConfigRequest_ClientInfo{
			SingboxVersion: singVersion(),
			ClientVersion:  app.ClientVersion,
			UserId:         strconv.FormatInt(f.user.LegacyID(), 10),
			ProToken:       f.user.LegacyToken(),
			Country:        "",
			Ip:             "",
		},
		PreferredRegion: preferredRegion,
		Proxy:           &ConfigRequest_Proxy{},
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
	req, err := backend.NewRequestWithHeaders(context.Background(), http.MethodPost, configURL, body, f.user)
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

// singVersion returns the version of the sing-box module.
func singVersion() string {
	// First look for the sagernet/sing-box module version, and if it's not found, look for the getlantern/sing-box module version.
	singVersion, err := moduleVersion("github.com/sagernet/sing-box", "github.com/getlantern/sing-box")
	if err != nil {
		singVersion = "unknown"
	}
	log.Debugf("sing-box version: %s", singVersion)
	return singVersion
}

func moduleVersion(modulePath ...string) (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", fmt.Errorf("could not read build info")
	}

	for _, mod := range info.Deps {
		if slices.Contains(modulePath, mod.Path) {
			return mod.Version, nil
		}
	}

	return "", fmt.Errorf("module %s not found", modulePath)
}
