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
	"go.opentelemetry.io/otel"

	"github.com/getlantern/sing-box-extensions/protocol"

	"github.com/getlantern/radiance/api"
	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/traces"
)

const configURL = "https://api.iantem.io/v1/config-new"
const tracerName = "github.com/getlantern/radiance/config"

type Fetcher interface {
	// fetchConfig fetches the configuration from the server. Nil is returned if no new config is available.
	// It returns an error if the request fails.
	// preferred is used to select the server location.
	// If preferred is empty, the server will select the best location.
	// The lastModified time is used to check if the configuration has changed since the last request.
	fetchConfig(ctx context.Context, preferred C.ServerLocation, wgPublicKey string) ([]byte, error)
}

// fetcher is responsible for fetching the configuration from the server.
type fetcher struct {
	httpClient   *http.Client
	user         common.UserInfo
	lastModified time.Time
	locale       string
	etag         string
	apiClient    *api.APIClient
}

// newFetcher creates a new fetcher with the given http client.
func newFetcher(client *http.Client, user common.UserInfo, locale string, apiClient *api.APIClient) Fetcher {
	return &fetcher{
		httpClient:   client,
		user:         user,
		lastModified: time.Time{},
		locale:       locale,
		apiClient:    apiClient,
	}
}

// fetchConfig fetches the configuration from the server. Nil is returned if no new config is available.
func (f *fetcher) fetchConfig(ctx context.Context, preferred C.ServerLocation, wgPublicKey string) ([]byte, error) {
	// If we don't have a user ID or token, create a new user.
	if f.user.LegacyID() == 0 || f.user.LegacyToken() == "" {
		f.apiClient.NewUser(ctx)
		slog.Info("Created new user", "id", f.user.LegacyID(), "token", f.user.LegacyToken())
	}
	confReq := C.ConfigRequest{
		SingboxVersion: singVersion(),
		Platform:       common.Platform,
		AppName:        common.Name,
		DeviceID:       f.user.DeviceID(),
		UserID:         fmt.Sprintf("%d", f.user.LegacyID()),
		ProToken:       f.user.LegacyToken(),
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

	slog.Debug("sending config request", "request", string(buf))
	buf, err = f.send(bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if buf == nil { // no new config available
		return nil, nil
	}
	slog.Log(nil, internal.LevelTrace, "received config", "config", string(buf))

	f.lastModified = time.Now()
	return buf, nil
}

// send sends a request to the server with the given body and returns the response.
func (f *fetcher) send(body io.Reader) ([]byte, error) {
	ctx, span := otel.Tracer(tracerName).Start(context.Background(), "config_fetcher.send")
	defer span.End()
	req, err := backend.NewRequestWithHeaders(ctx, http.MethodPost, configURL, body, f.user)
	if err != nil {
		return nil, fmt.Errorf("could not create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	// Add If-Modified-Since header to the request
	// Note that on the first run, lastModified is zero, so the server will return the latest config.
	req.Header.Set("If-Modified-Since", f.lastModified.Format(http.TimeFormat))
	if f.etag != "" {
		req.Header.Set("If-None-Match", f.etag)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("could not send request: %w", err))
	}
	defer resp.Body.Close()

	// Update the etag from the response
	if etag := resp.Header.Get("ETag"); etag != "" {
		f.etag = etag
	}

	// Note that Go's HTTP library should automatically have decompressed the response here.
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, traces.RecordError(ctx, fmt.Errorf("could not read response body: %w", err))
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
		slog.Debug("Config is not modified")
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
