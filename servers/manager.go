// Package servers provides management of server configurations, such as option.Endpoints and
// option.Outbounds,and integration with remote server managers for adding, inviting, and revoking
// private servers
package servers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

type ServerGroup = string

const (
	SGLantern ServerGroup = "lantern"
	SGUser    ServerGroup = "user"

	tracerName = "github.com/getlantern/radiance/servers"
)

// ServerCredentials holds the access token and invite status for a private server.
type ServerCredentials struct {
	AccessToken string `json:"access_token,omitempty"`
	Port        int    `json:"port,omitempty"`
	IsJoined    bool   `json:"is_joined,omitempty"` // whether the user has joined the server (i.e. accepted the invite)
}

type Options struct {
	Outbounds    []option.Outbound            `json:"outbounds,omitempty"`
	Endpoints    []option.Endpoint            `json:"endpoints,omitempty"`
	Locations    map[string]C.ServerLocation  `json:"locations,omitempty"`
	URLOverrides map[string]string            `json:"url_overrides,omitempty"`
	Credentials  map[string]ServerCredentials `json:"credentials,omitempty"`
}

// AllTags returns a slice of all tags from both endpoints and outbounds in the Options.
func (o Options) AllTags() []string {
	tags := make([]string, 0, len(o.Outbounds)+len(o.Endpoints))
	for _, ep := range o.Endpoints {
		tags = append(tags, ep.Tag)
	}
	for _, out := range o.Outbounds {
		tags = append(tags, out.Tag)
	}
	return tags
}

type Servers map[ServerGroup]Options

type Server struct {
	// Group indicates which group the server belongs to.
	Group ServerGroup
	// Tag is the tag/name of the server
	Tag string
	// Type is the type of the server, e.g. "http", "shadowsocks", etc.
	Type     string
	Options  any // will be either [option.Endpoint] or [option.Outbound]
	Location C.ServerLocation
}

type optsMap map[string]Server

func (m optsMap) add(group, tag, typ string, options any, loc C.ServerLocation) {
	m[tag] = Server{group, tag, typ, options, loc}
}

// Manager manages server configurations, including endpoints and outbounds.
type Manager struct {
	access  sync.RWMutex
	servers Servers
	optsMap optsMap // map of tag to option for quick access

	logger      *slog.Logger
	serversFile string
	httpClient  *http.Client
}

// NewManager creates a new Manager instance, loading server options from disk.
func NewManager(dataPath string, logger *slog.Logger) (*Manager, error) {
	mgr := &Manager{
		servers: Servers{
			SGLantern: Options{
				Outbounds:   make([]option.Outbound, 0),
				Endpoints:   make([]option.Endpoint, 0),
				Locations:   make(map[string]C.ServerLocation),
				Credentials: make(map[string]ServerCredentials),
			},
			SGUser: Options{
				Outbounds:   make([]option.Outbound, 0),
				Endpoints:   make([]option.Endpoint, 0),
				Locations:   make(map[string]C.ServerLocation),
				Credentials: make(map[string]ServerCredentials),
			},
		},
		optsMap:     map[string]Server{},
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
	mgr.logger.Log(nil, log.LevelTrace, "Loaded servers", "servers", mgr.servers)
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

// Servers returns the current server configurations for both groups ([SGLantern] and [SGUser]).
func (m *Manager) Servers() Servers {
	m.access.RLock()
	defer m.access.RUnlock()

	result := make(Servers, len(m.servers))
	for group, opts := range m.servers {
		result[group] = Options{
			Outbounds:    append([]option.Outbound{}, opts.Outbounds...),
			Endpoints:    append([]option.Endpoint{}, opts.Endpoints...),
			Locations:    maps.Clone(opts.Locations),
			URLOverrides: maps.Clone(opts.URLOverrides),
			Credentials:  maps.Clone(opts.Credentials),
		}
	}
	return result
}

// GetServerByTag returns the server configuration for a given tag and a boolean indicating whether
// the server was found. The returned Server contains pointer-rich sing-box types in its Options
// field, so callers on a CGo callback stack should use [GetServerByTagJSON] instead. This method
// does not use [common.RunOffCgoStack] because its only callers run on regular Go goroutines
// (event subscribers, private server flows), never on CGo callback stacks.
func (m *Manager) GetServerByTag(tag string) (Server, bool) {
	m.access.RLock()
	defer m.access.RUnlock()
	s, exists := m.optsMap[tag]
	return s, exists
}

// SetServers sets the server options for a specific group.
// Important: this will overwrite any existing servers for that group. To add new servers without
// overwriting existing ones, use [AddServers] instead.
func (m *Manager) SetServers(group ServerGroup, options Options) error {
	switch group {
	case SGLantern, SGUser:
	default:
		return fmt.Errorf("invalid server group: %s", group)
	}

	m.access.Lock()
	defer m.access.Unlock()
	if err := m.setServers(group, options); err != nil {
		return fmt.Errorf("set servers: %w", err)
	}

	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
	servers := make([]Server, 0, len(options.Outbounds)+len(options.Endpoints))
	for _, tag := range options.AllTags() {
		servers = append(servers, m.optsMap[tag])
	}
	return nil
}

func (m *Manager) setServers(group ServerGroup, options Options) error {
	m.logger.Log(nil, log.LevelTrace, "Setting servers", "group", group, "options", options)
	opts := Options{
		Outbounds:    append([]option.Outbound{}, options.Outbounds...),
		Endpoints:    append([]option.Endpoint{}, options.Endpoints...),
		Locations:    make(map[string]C.ServerLocation, len(options.Locations)),
		URLOverrides: maps.Clone(options.URLOverrides),
		Credentials:  make(map[string]ServerCredentials, len(options.Credentials)),
	}
	maps.Copy(opts.Locations, options.Locations)
	maps.Copy(opts.Credentials, options.Credentials)
	for _, ep := range opts.Endpoints {
		m.optsMap.add(group, ep.Tag, ep.Type, ep, options.Locations[ep.Tag])
	}
	for _, out := range opts.Outbounds {
		m.optsMap.add(group, out.Tag, out.Type, out, options.Locations[out.Tag])
	}
	m.servers[group] = opts
	return nil
}

// AddServers adds new servers to the specified group. If force is true, it will overwrite any
// existing servers with the same tags.
func (m *Manager) AddServers(group ServerGroup, options Options, force bool) error {
	switch group {
	case SGLantern, SGUser:
	default:
		return fmt.Errorf("invalid server group: %s", group)
	}
	if len(options.Endpoints) == 0 && len(options.Outbounds) == 0 {
		return nil
	}

	m.access.Lock()
	defer m.access.Unlock()

	m.logger.Log(nil, log.LevelTrace, "Adding servers", "group", group, "options", options)
	added := m.merge(group, options, force)
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
	m.logger.Info("Server configs added", "group", group, "newCount", len(added))
	return nil
}

func (m *Manager) merge(group ServerGroup, options Options, force bool) []Server {
	var added []Server
	servers := m.servers[group]
	for _, ep := range options.Endpoints {
		if !force {
			if _, exists := m.optsMap[ep.Tag]; exists {
				continue
			}
		}
		servers.Endpoints = append(servers.Endpoints, ep)
		servers.Locations[ep.Tag] = options.Locations[ep.Tag]
		m.optsMap.add(group, ep.Tag, ep.Type, ep, options.Locations[ep.Tag])
		added = append(added, m.optsMap[ep.Tag])
		if creds, ok := options.Credentials[ep.Tag]; ok {
			servers.Credentials[ep.Tag] = creds
		}
	}
	for _, out := range options.Outbounds {
		if !force {
			if _, exists := m.optsMap[out.Tag]; exists {
				continue
			}
		}
		servers.Outbounds = append(servers.Outbounds, out)
		servers.Locations[out.Tag] = options.Locations[out.Tag]
		m.optsMap.add(group, out.Tag, out.Type, out, options.Locations[out.Tag])
		added = append(added, m.optsMap[out.Tag])
		if creds, ok := options.Credentials[out.Tag]; ok {
			servers.Credentials[out.Tag] = creds
		}
	}
	for k, v := range options.URLOverrides {
		if servers.URLOverrides == nil {
			servers.URLOverrides = make(map[string]string)
		}
		servers.URLOverrides[k] = v
	}
	if force {
		servers.Endpoints = slices.CompactFunc(servers.Endpoints, func(ep1, ep2 option.Endpoint) bool {
			return ep1.Tag == ep2.Tag
		})
		servers.Outbounds = slices.CompactFunc(servers.Outbounds, func(ob1, ob2 option.Outbound) bool {
			return ob1.Tag == ob2.Tag
		})
	}
	m.servers[group] = servers
	return added
}

// RemoveServer removes a server config by its tag.
func (m *Manager) RemoveServer(tag string) error {
	_, err := m.removeServers([]string{tag})
	return err
}

// RemoveServers removes multiple server configs by their tags and returns the removed servers.
func (m *Manager) RemoveServers(tags []string) ([]Server, error) {
	return m.removeServers(tags)
}

func (m *Manager) removeServers(tags []string) ([]Server, error) {
	m.access.Lock()
	defer m.access.Unlock()

	removed := make([]Server, 0, len(tags))
	remove := func(it any) bool {
		var tag string
		switch v := it.(type) {
		case option.Endpoint:
			tag = v.Tag
		case option.Outbound:
			tag = v.Tag
		}
		server, exists := m.optsMap[tag]
		if exists {
			removed = append(removed, server)
		}
		return exists
	}
	for group, options := range m.servers {
		removed := removed[len(removed):]
		options.Outbounds = slices.DeleteFunc(options.Outbounds, func(out option.Outbound) bool {
			return remove(out)
		})
		options.Endpoints = slices.DeleteFunc(options.Endpoints, func(ep option.Endpoint) bool {
			return remove(ep)
		})
		for _, server := range removed {
			delete(options.Locations, server.Tag)
			delete(m.optsMap, server.Tag)
		}
		m.servers[group] = options
		if len(removed) > 0 {
			m.logger.Info("Server configs removed", "group", group, "tags", removed)
		}
	}

	if err := m.saveServers(); err != nil {
		return nil, fmt.Errorf("failed to save servers: %w", err)
	}
	return removed, nil
}

func (m *Manager) saveServers() error {
	m.logger.Log(nil, log.LevelTrace, "Saving server configs to file", "file", m.serversFile, "servers", m.servers)
	ctx := box.BaseContext()
	buf, err := json.MarshalContext(ctx, m.servers)
	if err != nil {
		return fmt.Errorf("marshal servers: %w", err)
	}
	return atomicfile.WriteFile(m.serversFile, buf, 0644)
}

func (m *Manager) loadServers() error {
	buf, err := atomicfile.ReadFile(m.serversFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil // file doesn't exist
	}
	if err != nil {
		return fmt.Errorf("read server file %q: %w", m.serversFile, err)
	}
	servers, err := json.UnmarshalExtendedContext[Servers](box.BaseContext(), buf)
	if err != nil {
		return fmt.Errorf("unmarshal server options: %w", err)
	}
	m.setServers(SGLantern, servers[SGLantern])
	m.setServers(SGUser, servers[SGUser])
	return nil
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

	ctx := box.BaseContext()
	servers, err := json.UnmarshalExtendedContext[Options](ctx, body)
	if err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(servers.Endpoints) == 0 && len(servers.Outbounds) == 0 {
		return fmt.Errorf("no endpoints or outbounds in response")
	}

	// TODO: update when we support endpoints
	servers.Outbounds[0].Tag = tag
	// If the server location is provided, set it for the server's tag.
	if loc != (C.ServerLocation{}) {
		servers.Locations = map[string]C.ServerLocation{
			tag: loc,
		}
	}
	// Store the credentials for the server's tag.
	servers.Credentials = map[string]ServerCredentials{
		tag: {AccessToken: accessToken, Port: port, IsJoined: joined},
	}
	slog.Info("Adding private server from remote manager", "tag", tag, "ip", ip, "port", port, "location", loc, "is_joined", joined)
	return m.AddServers(SGUser, servers, true)
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
	opts, err := json.UnmarshalExtendedContext[Options](box.BaseContext(), config)
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to parse config: %w", err))
	}
	if len(opts.Endpoints) == 0 && len(opts.Outbounds) == 0 {
		return traces.RecordError(ctx, fmt.Errorf("no endpoints or outbounds found in the provided configuration"))
	}
	if err := m.AddServers(SGUser, opts, true); err != nil {
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
