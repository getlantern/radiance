// Package servers provides management of server configurations, such as option.Endpoints and
// option.Outbounds,and integration with remote server managers for adding, inviting, and revoking
// private servers
package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	box "github.com/getlantern/lantern-box"
	"github.com/hashicorp/go-retryablehttp"
	"go.opentelemetry.io/otel"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/bypass"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
	"github.com/getlantern/radiance/traces"

	"github.com/getlantern/pluriconfig"
	"github.com/getlantern/pluriconfig/model"
	_ "github.com/getlantern/pluriconfig/provider/singbox"
	_ "github.com/getlantern/pluriconfig/provider/url"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

const tracerName = "github.com/getlantern/radiance/servers"

// ServerCredentials holds the access token and invite status for a private server.
type ServerCredentials struct {
	AccessToken string `json:"access_token,omitempty"`
	Port        int    `json:"port,omitempty"`
	IsJoined    bool   `json:"is_joined,omitempty"` // whether the user has joined the server (i.e. accepted the invite)
}

type Server struct {
	Tag           string             `json:"tag"`
	Type          string             `json:"type"`
	IsLantern     bool               `json:"isLantern"`
	Options       any                `json:"options"`
	Location      C.ServerLocation   `json:"location,omitempty"`
	Credentials   *ServerCredentials `json:"credentials,omitempty"`
	URLTestResult *URLTestResult     `json:"urlTestResult,omitempty"`
}

// serverJSON is the on-wire representation of a Server. The Options field is split into
// explicit Outbound/Endpoint fields so that the sing-box context-aware JSON marshaler can
// properly serialize/deserialize the typed options (e.g. SamizdatOutboundOptions).
type serverJSON struct {
	Tag           string             `json:"tag"`
	Type          string             `json:"type"`
	IsLantern     bool               `json:"isLantern"`
	Outbound      *option.Outbound   `json:"outbound,omitempty"`
	Endpoint      *option.Endpoint   `json:"endpoint,omitempty"`
	Location      C.ServerLocation   `json:"location,omitempty"`
	Credentials   *ServerCredentials `json:"credentials,omitempty"`
	URLTestResult *URLTestResult     `json:"urlTestResult,omitempty"`
}

func (s Server) MarshalJSON() ([]byte, error) {
	sj := serverJSON{
		Tag:           s.Tag,
		Type:          s.Type,
		IsLantern:     s.IsLantern,
		Location:      s.Location,
		Credentials:   s.Credentials,
		URLTestResult: s.URLTestResult,
	}
	switch opts := s.Options.(type) {
	case option.Outbound:
		sj.Outbound = &opts
	case option.Endpoint:
		sj.Endpoint = &opts
	}
	return json.MarshalContext(box.BaseContext(), sj)
}

func (s *Server) UnmarshalJSON(data []byte) error {
	sj, err := json.UnmarshalExtendedContext[serverJSON](box.BaseContext(), data)
	if err != nil {
		return err
	}
	s.Tag = sj.Tag
	s.Type = sj.Type
	s.IsLantern = sj.IsLantern
	s.Location = sj.Location
	s.Credentials = sj.Credentials
	s.URLTestResult = sj.URLTestResult
	if sj.Outbound != nil {
		s.Options = *sj.Outbound
	} else if sj.Endpoint != nil {
		s.Options = *sj.Endpoint
	}
	return nil
}

// ServerList is a batch of servers with optional URL overrides for bulk operations.
type ServerList struct {
	Servers      []*Server         `json:"servers"`
	URLOverrides map[string]string `json:"url_overrides,omitempty"`
}

func (sl ServerList) Tags() []string {
	tags := make([]string, 0, len(sl.Servers))
	for _, s := range sl.Servers {
		tags = append(tags, s.Tag)
	}
	return tags
}

func (sl ServerList) Outbounds() []option.Outbound {
	var out []option.Outbound
	for _, s := range sl.Servers {
		if o, ok := s.Options.(option.Outbound); ok {
			out = append(out, o)
		}
	}
	return out
}

func (sl ServerList) Endpoints() []option.Endpoint {
	var eps []option.Endpoint
	for _, s := range sl.Servers {
		if e, ok := s.Options.(option.Endpoint); ok {
			eps = append(eps, e)
		}
	}
	return eps
}

// Manager manages server configurations, including endpoints and outbounds.
type Manager struct {
	access  sync.RWMutex
	servers map[string]*Server // tag -> Server

	logger      *slog.Logger
	serversFile string
	httpClient  *http.Client
}

// NewManager creates a new Manager instance, loading server options from disk.
func NewManager(dataPath string, logger *slog.Logger) (*Manager, error) {
	mgr := &Manager{
		servers:     make(map[string]*Server),
		serversFile: filepath.Join(dataPath, internal.ServersFileName),
		logger:      logger,
		// Use the bypass proxy dialer to route requests outside the VPN tunnel.
		// This client is only used to access private servers the user has created.
		httpClient: retryableHTTPClient(logger).StandardClient(),
	}

	mgr.logger.Debug("Loading servers", "file", mgr.serversFile)
	if err := mgr.loadServers(); err != nil {
		mgr.logger.Error("Failed to load servers", "file", mgr.serversFile, "error", err)
		return nil, fmt.Errorf("failed to load servers from file: %w", err)
	}
	mgr.logger.Log(nil, log.LevelTrace, "Loaded servers")
	return mgr, nil
}

func retryableHTTPClient(logger *slog.Logger) *retryablehttp.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           bypass.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	client := retryablehttp.NewClient()
	client.HTTPClient = &http.Client{
		Transport: transport,
	}

	client.RetryMax = 10
	client.RetryWaitMin = 1 * time.Second
	client.RetryWaitMax = 10 * time.Second
	client.Logger = logger
	return client
}

// AllServers returns a deep-copied slice of all servers.
func (m *Manager) AllServers() []*Server {
	m.access.RLock()
	defer m.access.RUnlock()
	result := make([]*Server, 0, len(m.servers))
	for _, srv := range m.servers {
		cp := *srv
		result = append(result, &cp)
	}
	return result
}

// URLTestResult holds the result of a single URL test.
type URLTestResult struct {
	Delay uint16    `json:"delay"`
	Time  time.Time `json:"time"`
}

// UpdateURLTestResults updates the URL test results for servers matching the provided tags.
func (m *Manager) UpdateURLTestResults(results map[string]URLTestResult) {
	m.access.Lock()
	defer m.access.Unlock()
	for tag, result := range results {
		if srv, exists := m.servers[tag]; exists {
			r := result
			srv.URLTestResult = &r
		}
	}
}

// GetServerByTag returns the server configuration for a given tag and a boolean indicating whether
// the server was found.
func (m *Manager) GetServerByTag(tag string) (*Server, bool) {
	m.access.RLock()
	defer m.access.RUnlock()
	s, exists := m.servers[tag]
	if !exists {
		return nil, false
	}
	cp := *s
	return &cp, true
}

// SetServers sets the server options for servers with a matching IsLantern value.
// Important: this will overwrite any existing servers with the same IsLantern value. To add new
// servers without overwriting existing ones, use [AddServers] instead.
func (m *Manager) SetServers(isLantern bool, list ServerList) error {
	m.access.Lock()
	defer m.access.Unlock()
	// Remove existing with matching IsLantern
	for tag, srv := range m.servers {
		if srv.IsLantern == isLantern {
			delete(m.servers, tag)
		}
	}
	// Add new
	for _, srv := range list.Servers {
		srv.IsLantern = isLantern
		m.servers[srv.Tag] = srv
	}
	return m.saveServers()
}

// AddServers adds new servers. If force is true, it will overwrite any
// existing servers with the same tags.
func (m *Manager) AddServers(isLantern bool, list ServerList, force bool) error {
	if len(list.Servers) == 0 {
		return nil
	}

	m.access.Lock()
	defer m.access.Unlock()

	for _, srv := range list.Servers {
		srv.IsLantern = isLantern
		if !force {
			if _, exists := m.servers[srv.Tag]; exists {
				continue
			}
		}
		m.servers[srv.Tag] = srv
	}
	return m.saveServers()
}

// RemoveServer removes a server config by its tag.
func (m *Manager) RemoveServer(tag string) error {
	_, err := m.RemoveServers([]string{tag})
	return err
}

// RemoveServers removes multiple server configs by their tags and returns the removed servers.
func (m *Manager) RemoveServers(tags []string) ([]*Server, error) {
	m.access.Lock()
	defer m.access.Unlock()
	removed := make([]*Server, 0, len(tags))
	for _, tag := range tags {
		if srv, exists := m.servers[tag]; exists {
			removed = append(removed, srv)
			delete(m.servers, tag)
		}
	}
	if err := m.saveServers(); err != nil {
		return nil, fmt.Errorf("failed to save servers: %w", err)
	}
	return removed, nil
}

func (m *Manager) saveServers() error {
	servers := make([]*Server, 0, len(m.servers))
	for _, srv := range m.servers {
		servers = append(servers, srv)
	}
	buf, err := json.MarshalContext(box.BaseContext(), servers)
	if err != nil {
		return fmt.Errorf("marshal servers: %w", err)
	}
	return atomicfile.WriteFile(m.serversFile, buf, 0644)
}

const (
	modeLantern = "lantern"
	modeUser    = "user"
)

func (m *Manager) loadServers() error {
	buf, err := atomicfile.ReadFile(m.serversFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil // file doesn't exist
	}
	if err != nil {
		return fmt.Errorf("read server file %q: %w", m.serversFile, err)
	}
	buf = bytes.TrimSpace(buf)
	ctx := box.BaseContext()

	if len(buf) > 0 && buf[0] == '[' {
		loaded, err := json.UnmarshalExtendedContext[[]*Server](ctx, buf)
		if err != nil {
			return fmt.Errorf("unmarshal servers: %w", err)
		}
		for _, srv := range loaded {
			m.servers[srv.Tag] = srv
		}
		return nil
	}

	// Fall back to old format: map[string]Options and mirgrate to new format on save.
	type oldOptions struct {
		Outbounds   []option.Outbound            `json:"outbounds,omitempty"`
		Endpoints   []option.Endpoint            `json:"endpoints,omitempty"`
		Locations   map[string]C.ServerLocation  `json:"locations,omitempty"`
		Credentials map[string]ServerCredentials `json:"credentials,omitempty"`
	}
	old, err := json.UnmarshalExtendedContext[map[string]oldOptions](ctx, buf)
	if err != nil {
		return fmt.Errorf("unmarshal server options: %w", err)
	}
	for group, opts := range old {
		isLantern := group == modeLantern
		for _, out := range opts.Outbounds {
			srv := &Server{
				Tag: out.Tag, Type: out.Type, IsLantern: isLantern,
				Options: out, Location: opts.Locations[out.Tag],
			}
			if creds, ok := opts.Credentials[out.Tag]; ok {
				srv.Credentials = &creds
			}
			m.servers[out.Tag] = srv
		}
		for _, ep := range opts.Endpoints {
			srv := &Server{
				Tag: ep.Tag, Type: ep.Type, IsLantern: isLantern,
				Options: ep, Location: opts.Locations[ep.Tag],
			}
			if creds, ok := opts.Credentials[ep.Tag]; ok {
				srv.Credentials = &creds
			}
			m.servers[ep.Tag] = srv
		}
	}
	// Re-save in new format
	return m.saveServers()
}

// Lantern Server Manager Integration

// AddPrivateServer fetches VPN connection info from a remote server manager and adds it as a server.
func (m *Manager) AddPrivateServer(tag, ip string, port int, accessToken string, loc C.ServerLocation, joined bool) error {
	u := &url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(ip, strconv.Itoa(port)),
		Path:   "/api/v1/connect-config",
	}
	q := u.Query()
	q.Set("token", accessToken)
	u.RawQuery = q.Encode()
	resp, err := m.httpClient.Get(u.String())
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to get connect config, unexpected status: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	type remoteConfig struct {
		Outbounds []option.Outbound `json:"outbounds,omitempty"`
		Endpoints []option.Endpoint `json:"endpoints,omitempty"`
	}
	ctx := box.BaseContext()
	cfg, err := json.UnmarshalExtendedContext[remoteConfig](ctx, body)
	if err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(cfg.Endpoints) == 0 && len(cfg.Outbounds) == 0 {
		return fmt.Errorf("no endpoints or outbounds in response")
	}

	// TODO: update when we support endpoints
	cfg.Outbounds[0].Tag = tag
	srv := &Server{
		Tag:       tag,
		Type:      cfg.Outbounds[0].Type,
		IsLantern: false,
		Options:   cfg.Outbounds[0],
		Location:  loc,
		Credentials: &ServerCredentials{
			AccessToken: accessToken, Port: port, IsJoined: joined,
		},
	}
	slog.Info("Adding private server from remote manager", "tag", tag, "ip", ip, "port", port, "location", loc, "is_joined", joined)
	list := ServerList{Servers: []*Server{srv}}
	return m.AddServers(false, list, true)
}

// InviteToPrivateServer invites another user to the server manager instance and returns a connection
// token. The server must be added to the user's servers first.
func (m *Manager) InviteToPrivateServer(ip string, port int, accessToken string, inviteName string) (string, error) {
	resp, err := m.httpClient.Get(fmt.Sprintf("https://%s:%d/api/v1/share-link/%s?token=%s", ip, port, inviteName, accessToken))
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to get connect config, invalid status code: %d", resp.StatusCode)
	}
	type tokenResp struct {
		Token string
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	var cs tokenResp
	if err = json.Unmarshal(body, &cs); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return cs.Token, nil
}

// RevokePrivateServerInvite will revoke an invite to the server manager instance. The server must
// be added to the user's servers first.
func (m *Manager) RevokePrivateServerInvite(ip string, port int, accessToken string, inviteName string) error {
	resp, err := m.httpClient.Post(fmt.Sprintf("https://%s:%d/api/v1/revoke/%s?token=%s", ip, port, inviteName, accessToken), "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to revoke invite, invalid status code: %d", resp.StatusCode)
	}
	return nil
}

// AddServersByJSON adds any outbounds and endpoints defined in the provided sing-box JSON config.
func (m *Manager) AddServersByJSON(ctx context.Context, config []byte) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "Manager.AddServerBySingboxJSON")
	defer span.End()
	type singboxConfig struct {
		Outbounds []option.Outbound `json:"outbounds,omitempty"`
		Endpoints []option.Endpoint `json:"endpoints,omitempty"`
	}
	cfg, err := json.UnmarshalExtendedContext[singboxConfig](box.BaseContext(), config)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to parse config: %w", err))
	}
	if len(cfg.Endpoints) == 0 && len(cfg.Outbounds) == 0 {
		return traces.RecordError(ctx, fmt.Errorf("no endpoints or outbounds found in the provided configuration"))
	}
	var servers []*Server
	for _, out := range cfg.Outbounds {
		servers = append(servers, &Server{Tag: out.Tag, Type: out.Type, Options: out})
	}
	for _, ep := range cfg.Endpoints {
		servers = append(servers, &Server{Tag: ep.Tag, Type: ep.Type, Options: ep})
	}
	if err := m.AddServers(false, ServerList{Servers: servers}, true); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to add servers: %w", err))
	}
	return nil
}

// AddServersByURL adds a server(s) by downloading and parsing the config from a list of URLs.
func (m *Manager) AddServersByURL(ctx context.Context, urls []string, skipCertVerification bool) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "Manager.AddServerByURLs")
	defer span.End()
	urlProvider, loaded := pluriconfig.GetProvider(string(model.ProviderURL))
	if !loaded {
		return traces.RecordError(ctx, fmt.Errorf("URL config provider not loaded"))
	}
	cfg, err := urlProvider.Parse(ctx, []byte(strings.Join(urls, "\n")))
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to parse URLs: %w", err))
	}
	cfgURLs, ok := cfg.Options.([]url.URL)
	if !ok || len(cfgURLs) == 0 {
		return traces.RecordError(ctx, fmt.Errorf("no valid URLs found in the provided configuration"))
	}

	if skipCertVerification {
		urlsWithCustomOptions := make([]url.URL, 0, len(cfgURLs))
		for _, v := range cfgURLs {
			queryParams := v.Query()
			queryParams.Add("allowInsecure", "1")
			v.RawQuery = queryParams.Encode()
			urlsWithCustomOptions = append(urlsWithCustomOptions, v)
		}
		cfg.Options = urlsWithCustomOptions
	}

	singBoxProvider, loaded := pluriconfig.GetProvider(string(model.ProviderSingBox))
	if !loaded {
		return traces.RecordError(ctx, fmt.Errorf("singbox config provider not loaded"))
	}
	singBoxCfg, err := singBoxProvider.Serialize(ctx, cfg)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to serialize sing-box config: %w", err))
	}
	m.logger.Info("Added servers based on URLs", "serverCount", len(cfgURLs), "skipCertVerification", skipCertVerification)
	return m.AddServersByJSON(ctx, singBoxCfg)
}
