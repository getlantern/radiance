package config

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"slices"

	C "github.com/getlantern/common"

	"github.com/getlantern/sing-box-extensions/protocol"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
)

const configURL = "https://api.iantem.io/v1/config-new"

type Fetcher interface {
	// fetchConfig fetches the configuration from the server. Nil is returned if no new config is available.
	// It returns an error if the request fails.
	// preferred is used to select the server location.
	// If preferred is empty, the server will select the best location.
	// The lastModified time is used to check if the configuration has changed since the last request.
	fetchConfig(preferred C.ServerLocation, wgPublicKey string) ([]byte, error)
}

// fetcher is responsible for fetching the configuration from the server.
type fetcher struct {
	httpClient   *http.Client
	user         common.UserInfo
	lastModified time.Time
	locale       string
}

// newFetcher creates a new fetcher with the given http client.
func newFetcher(client *http.Client, user common.UserInfo, locale string) Fetcher {
	return &fetcher{
		httpClient:   client,
		user:         user,
		lastModified: time.Time{},
		locale:       locale,
	}
}

// fetchConfig fetches the configuration from the server. Nil is returned if no new config is available.
func (f *fetcher) fetchConfig(preferred C.ServerLocation, wgPublicKey string) ([]byte, error) {
	confReq := C.ConfigRequest{
		SingboxVersion: singVersion(),
		Platform:       common.Platform,
		AppName:        common.Name,
		DeviceID:       f.user.DeviceID(),
		WGPublicKey:    wgPublicKey,
		Backend:        C.SINGBOX,
		Locale:         f.locale,
		Protocols:      protocol.SupportedProtocols(),
	}
	if preferred.Country != "" {
		confReq.PreferredLocation = &preferred
	}
	buf, err := json.Marshal(&confReq)
	if err != nil {
		return nil, fmt.Errorf("marshal config request: %w", err)
	}

	slog.Info("fetching config", "request", string(buf))
	buf, err = f.send(bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if buf == nil { // no new config available
		return nil, nil
	}
	slog.Info("fetched config", "config", string(buf))

	f.lastModified = time.Now()
	return buf, nil
}

// send sends a request to the server with the given body and returns the response.
func (f *fetcher) send(body io.Reader) ([]byte, error) {
	req, err := backend.NewRequestWithHeaders(context.Background(), http.MethodPost, configURL, body, f.user)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	// Add If-Modified-Since header to the request
	// Note that on the first run, lastModified is zero, so the server will return the latest config.
	req.Header.Set("If-Modified-Since", f.lastModified.Format(http.TimeFormat))

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not send request: %w", err)
	}

	// Note that Go's HTTP library should automatically have decompressed the response here.
	buf, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("could not read response body: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		// 200 OK
		return buf, nil
	case http.StatusPartialContent:
		// 206 Partial Content
		return buf, nil
	case http.StatusNotModified:
		// 304 Not Modified
		return nil, nil
	case http.StatusNoContent:
		// 204 No Content
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected status code: %d. %s", resp.StatusCode, buf)
	}
}

// singVersion returns the version of the sing-box module.
func singVersion() string {
	// First look for the sagernet/sing-box module version, and if it's not found, look for the getlantern/sing-box module version.
	singVersion, err := moduleVersion("github.com/sagernet/sing-box", "github.com/getlantern/sing-box")
	if err != nil {
		singVersion = "unknown"
	}
	slog.Debug("sing-box version", "version", singVersion)
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
