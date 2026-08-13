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

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
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

// HeartbeatRequest is LifecycleRequest plus the peer's current load. Separate
// from LifecycleRequest so verify and deregister, which have no load to
// report, don't carry a field that would read as "zero clients".
type HeartbeatRequest struct {
	RouteID string `json:"route_id"`
	// ActiveClients counts distinct remote IPs with at least one open
	// connection, not connections: samizdat multiplexes many H2 streams over
	// one TCP conn, and a client may hold several, so a connection count would
	// overstate how many people are actually being helped.
	ActiveClients int `json:"active_clients"`
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

// NewAPI constructs the client. baseURL must already include the API
// version path segment — common.GetBaseURL() returns an env-specific URL
// suffixed with either ".../v1" or ".../api/v1". Per-endpoint paths are
// appended to baseURL without re-adding any version segment, matching
// how every other radiance caller of common.GetBaseURL composes URLs.
func NewAPI(httpClient *http.Client, baseURL, deviceID string) *API {
	return &API{httpClient: httpClient, baseURL: baseURL, deviceID: deviceID}
}

func (a *API) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := a.do(ctx, http.MethodPost, "/peer/register", req, &resp); err != nil {
		return nil, fmt.Errorf("register: %w", err)
	}
	return &resp, nil
}

// Verify asks lantern-cloud to dial the peer's external endpoint through a
// freshly-built samizdat client. Called after Start has finished bringing
// up sing-box locally so the server's verifier hits a live listener with
// the matching creds. Server-side failure deprecates the row + returns
// 422; the caller treats that as a fatal Start error and tears down.
func (a *API) Verify(ctx context.Context, routeID string) error {
	if err := a.do(ctx, http.MethodPost, "/peer/verify", LifecycleRequest{RouteID: routeID}, nil); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	return nil
}

// Heartbeat extends the peer route's TTL and reports how many distinct client
// devices are currently connected. The server owner-gates via
// X-Lantern-Device-Id, so a leaked route_id can't be used by another device
// to keep the registration alive.
//
// activeClients lets the server stop handing this peer to new clients once it
// is carrying its share. It is reported rather than inferred because the
// server has no other view of it: assignment records only exist for WireGuard
// variants, and a peer serves samizdat. Reporting also self-corrects — a count
// measured now cannot go stale the way an assignment log does when clients
// simply disappear.
func (a *API) Heartbeat(ctx context.Context, routeID string, activeClients int) error {
	req := HeartbeatRequest{RouteID: routeID, ActiveClients: activeClients}
	if err := a.do(ctx, http.MethodPost, "/peer/heartbeat", req, nil); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (a *API) Deregister(ctx context.Context, routeID string) error {
	if err := a.do(ctx, http.MethodPost, "/peer/deregister", LifecycleRequest{RouteID: routeID}, nil); err != nil {
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
	// Use common.NewRequestWithHeaders so peer endpoints carry the same
	// header set as /config-new — most importantly X-Lantern-Config-Client-IP,
	// which the server's util.ClientIPWithAddr prefers over X-Forwarded-For
	// and RemoteAddr. Without it, register/verify can resolve a different
	// IP than radiance has detected as the client's public IP, and the
	// server's verifier dials an address the peer's listener isn't bound to.
	r, err := common.NewRequestWithHeaders(ctx, method, a.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	// NewRequestWithHeaders sets DeviceIDHeader from settings; override with
	// the API's bound deviceID for parity with the prior behavior in case
	// the two ever diverge.
	r.Header.Set(common.DeviceIDHeader, a.deviceID)
	// Forward the same feature-override header that config/fetcher.go uses
	// for /config-new requests, so QA can flip on `peer_proxy` ahead of the
	// public-flag rollout via FeatureOverridesKey (RADIANCE_FEATURE_OVERRIDES).
	// Without this the server-side gate rejects register/heartbeat/deregister
	// regardless of the local toggle.
	if val := settings.GetString(settings.FeatureOverridesKey); val != "" {
		r.Header.Set("X-Lantern-Feature-Override", val)
	}

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
