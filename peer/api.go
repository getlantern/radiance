// Package peer implements the client side of "Share My Connection". api.go
// is the thin HTTP client for lantern-cloud's /v1/peer/* endpoints.
package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type RegisterRequest struct {
	ExternalIP   string `json:"external_ip"`
	ExternalPort uint16 `json:"external_port"`
	InternalPort uint16 `json:"internal_port"`
}

type RegisterResponse struct {
	RouteID                  string `json:"route_id"`
	ServerConfig             string `json:"server_config"`
	HeartbeatIntervalSeconds int64  `json:"heartbeat_interval_seconds"`
}

type LifecycleRequest struct {
	RouteID string `json:"route_id"`
}

// APIError carries the server's HTTP status and body. Callers map specific
// statuses to user-facing errors (404 → not registered, 422 → not reachable
// from the public internet, 503 → feature off).
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("peer api: status=%d body=%s", e.Status, e.Body)
}

type API struct {
	httpClient *http.Client
	baseURL    string
	deviceID   string
}

// NewAPI constructs the client. baseURL must not have a trailing slash and
// must not include "/v1" — that's appended per-endpoint.
func NewAPI(httpClient *http.Client, baseURL, deviceID string) *API {
	return &API{httpClient: httpClient, baseURL: baseURL, deviceID: deviceID}
}

func (a *API) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := a.do(ctx, http.MethodPost, "/v1/peer/register", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	return &resp, nil
}

// Heartbeat extends the peer route's TTL. The server owner-gates via
// X-Lantern-Device-Id, so a leaked route_id can't be used by another device
// to keep the registration alive.
func (a *API) Heartbeat(ctx context.Context, routeID string) error {
	if err := a.do(ctx, http.MethodPost, "/v1/peer/heartbeat", LifecycleRequest{RouteID: routeID}, nil); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (a *API) Deregister(ctx context.Context, routeID string) error {
	if err := a.do(ctx, http.MethodPost, "/v1/peer/deregister", LifecycleRequest{RouteID: routeID}, nil); err != nil {
		return fmt.Errorf("deregister: %w", err)
	}
	return nil
}

func (a *API) do(ctx context.Context, method, path string, body, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}
	r, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("X-Lantern-Device-Id", a.deviceID)

	resp, err := a.httpClient.Do(r)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		const maxBody = 4096
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		return &APIError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(buf))}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
