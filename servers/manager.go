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
	"sync"
	"time"

	box "github.com/getlantern/lantern-box"
	"github.com/hashicorp/go-retryablehttp"
	"go.opentelemetry.io/otel"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/atomicfile"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/internal"
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

type Options struct {
	Outbounds []option.Outbound           `json:"outbounds,omitempty"`
	Endpoints []option.Endpoint           `json:"endpoints,omitempty"`
	Locations map[string]C.ServerLocation `json:"locations,omitempty"`
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

// Manager manages server configurations, including endpoints and outbounds.
type Manager struct {
	access   sync.RWMutex
	servers  Servers
	optsMaps map[ServerGroup]map[string]any // map of tag to option for quick access

	serversFile string
	httpClient  *http.Client
}

// NewManager creates a new Manager instance, loading server options from disk.
func NewManager(dataPath string) (*Manager, error) {
	mgr := &Manager{
		servers: Servers{
			SGLantern: Options{
				Outbounds: make([]option.Outbound, 0),
				Endpoints: make([]option.Endpoint, 0),
				Locations: make(map[string]C.ServerLocation),
			},
			SGUser: Options{
				Outbounds: make([]option.Outbound, 0),
				Endpoints: make([]option.Endpoint, 0),
				Locations: make(map[string]C.ServerLocation),
			},
		},
		optsMaps: map[ServerGroup]map[string]any{
			SGLantern: make(map[string]any),
			SGUser:    make(map[string]any),
		},
		serversFile: filepath.Join(dataPath, common.ServersFileName),
		access:      sync.RWMutex{},

		// Note that we use a regular http.Client here because it is only used to access private
		// servers the user has created.
		// Use the same configuration as http.DefaultClient.
		httpClient: retryableHTTPClient().StandardClient(),
	}

	slog.Debug("Loading servers", "file", mgr.serversFile)
	if err := mgr.loadServers(); err != nil {
		slog.Error("Failed to load servers", "file", mgr.serversFile, "error", err)
		return nil, fmt.Errorf("failed to load servers from file: %w", err)
	}
	slog.Log(nil, internal.LevelTrace, "Loaded servers", "servers", mgr.servers)
	return mgr, nil
}

func retryableHTTPClient() *retryablehttp.Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
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
	return client
}

// Servers returns the current server configurations for both groups ([SGLantern] and [SGUser]).
func (m *Manager) Servers() Servers {
	m.access.RLock()
	defer m.access.RUnlock()

	result := make(Servers, len(m.servers))
	for group, opts := range m.servers {
		result[group] = Options{
			Outbounds: append([]option.Outbound{}, opts.Outbounds...),
			Endpoints: append([]option.Endpoint{}, opts.Endpoints...),
			Locations: maps.Clone(opts.Locations),
		}
	}
	return result
}

type Server struct {
	Group    ServerGroup
	Tag      string
	Type     string
	Options  any // will be either [option.Endpoint] or [option.Outbound]
	Location C.ServerLocation
}

// GetServerByTag returns the server configuration for a given tag and a boolean indicating whether
// the server was found.
func (m *Manager) GetServerByTag(tag string) (Server, bool) {
	m.access.RLock()
	defer m.access.RUnlock()

	group := SGLantern
	opts, ok := m.optsMaps[SGLantern][tag]
	if !ok {
		if opts, ok = m.optsMaps[SGUser][tag]; !ok {
			return Server{}, false
		}
		group = SGUser
	}
	s := Server{
		Group:    group,
		Tag:      tag,
		Options:  opts,
		Location: m.servers[group].Locations[tag],
	}
	switch v := opts.(type) {
	case option.Endpoint:
		s.Type = v.Type
	case option.Outbound:
		s.Type = v.Type
	}
	return s, true
}

type ServersUpdatedEvent struct {
	events.Event
	Group   ServerGroup
	Options *Options
}

type ServersAddedEvent struct {
	events.Event
	Group   ServerGroup
	Options *Options
}

type ServersRemovedEvent struct {
	events.Event
	Group ServerGroup
	Tag   string
}

// SetServers sets the server options for a specific group.
// Important: this will overwrite any existing servers for that group. To add new servers without
// overwriting existing ones, use [AddServers] instead.
func (m *Manager) SetServers(group ServerGroup, options Options) error {
	if err := m.setServers(group, options); err != nil {
		return fmt.Errorf("set servers: %w", err)
	}

	m.access.Lock()
	defer m.access.Unlock()
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
	events.Emit(ServersUpdatedEvent{
		Group:   group,
		Options: &options,
	})
	return nil
}

func (m *Manager) setServers(group ServerGroup, options Options) error {
	switch group {
	case SGLantern, SGUser:
	default:
		return fmt.Errorf("invalid server group: %s", group)
	}

	m.access.Lock()
	defer m.access.Unlock()

	slog.Log(nil, internal.LevelTrace, "Setting servers", "group", group, "options", options)
	opts := Options{
		Outbounds: append([]option.Outbound{}, options.Outbounds...),
		Endpoints: append([]option.Endpoint{}, options.Endpoints...),
		Locations: make(map[string]C.ServerLocation, len(options.Locations)),
	}
	if len(options.Locations) > 0 {
		maps.Copy(opts.Locations, options.Locations)
	}

	m.servers[group] = opts
	oMap := make(map[string]any, len(options.Endpoints)+len(options.Outbounds))
	for _, ep := range options.Endpoints {
		oMap[ep.Tag] = ep
	}
	for _, out := range options.Outbounds {
		oMap[out.Tag] = out
	}
	m.optsMaps[group] = oMap
	return nil
}

// AddServers adds new servers to the specified group. If a server with the same tag already exists,
// it will be skipped.
func (m *Manager) AddServers(group ServerGroup, opts Options) error {
	switch group {
	case SGLantern, SGUser:
	default:
		return fmt.Errorf("invalid server group: %s", group)
	}

	m.access.Lock()
	defer m.access.Unlock()

	slog.Log(nil, internal.LevelTrace, "Adding servers", "group", group, "options", opts)
	existingTags := m.merge(group, opts)
	if len(existingTags) > 0 {
		slog.Warn("Some servers were not added because they already exist", "tags", existingTags)
	}
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
	if len(existingTags) > 0 {
		slog.Warn("Tried to add some servers that already exist", "tags", existingTags)
		return fmt.Errorf("some servers were not added because they already exist: %v", existingTags)
	}
	slog.Debug("Server configs added", "group", group, "newCount", len(opts.AllTags()))
	events.Emit(ServersAddedEvent{
		Group:   group,
		Options: &opts,
	})
	return nil
}

// merge adds new endpoints and outbounds to the specified group, skipping any that already exist.
// It returns the tags that were skipped.
func (m *Manager) merge(group ServerGroup, options Options) []string {
	if len(options.Endpoints) == 0 && len(options.Outbounds) == 0 {
		return nil
	}
	var existingTags []string
	opts := m.optsMaps[group]
	servers := m.servers[group]
	for _, ep := range options.Endpoints {
		if _, exists := opts[ep.Tag]; exists {
			existingTags = append(existingTags, ep.Tag)
			continue
		}
		opts[ep.Tag] = ep
		servers.Endpoints = append(servers.Endpoints, ep)
		servers.Locations[ep.Tag] = options.Locations[ep.Tag]
	}
	for _, out := range options.Outbounds {
		if _, exists := opts[out.Tag]; exists {
			existingTags = append(existingTags, out.Tag)
			continue
		}
		opts[out.Tag] = out
		servers.Outbounds = append(servers.Outbounds, out)
		servers.Locations[out.Tag] = options.Locations[out.Tag]
	}
	m.servers[group] = servers
	return existingTags
}

// RemoveServer removes a server config by its tag.
func (m *Manager) RemoveServer(tag string) error {
	m.access.Lock()
	defer m.access.Unlock()

	slog.Log(nil, internal.LevelTrace, "Removing server", "tag", tag)
	// check which group the server belongs to so we can get the correct optsMaps and servers
	group := SGLantern
	if _, exists := m.optsMaps[group][tag]; !exists {
		group = SGUser
		if _, exists := m.optsMaps[group][tag]; !exists {
			slog.Warn("Tried to remove non-existent server", "tag", tag)
			return fmt.Errorf("server with tag %q not found", tag)
		}
	}
	// remove the server from the optsMaps and servers
	servers := m.servers[group]
	switch v := m.optsMaps[group][tag].(type) {
	case option.Endpoint:
		servers.Endpoints = remove(servers.Endpoints, v)
	case option.Outbound:
		servers.Outbounds = remove(servers.Outbounds, v)
	}
	delete(m.optsMaps[group], tag)
	delete(servers.Locations, tag)
	m.servers[group] = servers
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers after removing %q: %w", tag, err)
	}
	slog.Debug("Server config removed", "group", group, "tag", tag)
	events.Emit(ServersRemovedEvent{
		Group: group,
		Tag:   tag,
	})
	return nil
}

func remove[T comparable](slice []T, item T) []T {
	i := slices.Index(slice, item)
	if i == -1 {
		return slice
	}
	slice[i] = slice[len(slice)-1]
	return slice[:len(slice)-1]
}

func (m *Manager) saveServers() error {
	slog.Log(nil, internal.LevelTrace, "Saving server configs to file", "file", m.serversFile, "servers", m.servers)
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
func (m *Manager) AddPrivateServer(tag string, ip string, port int, accessToken string) error {
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
	servers.Outbounds[0].Tag = tag // use the provided tag
	return m.AddServers(SGUser, servers)
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

// AddServerWithSingboxJSON parse a value that can be a JSON sing-box config.
// It parses the config into a sing-box config and add it to the user managed group.
func (m *Manager) AddServerWithSingboxJSON(ctx context.Context, value []byte) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "Manager.AddServerWithSingboxJSON")
	defer span.End()
	var opts Options
	if err := json.UnmarshalContext(box.BaseContext(), value, &opts); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to parse config: %w", err))
	}
	if len(opts.Endpoints) == 0 && len(opts.Outbounds) == 0 {
		return traces.RecordError(ctx, fmt.Errorf("no endpoints or outbounds found in the provided configuration"))
	}
	if err := m.AddServers(SGUser, opts); err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to add servers: %w", err))
	}
	return nil
}

// AddServerBasedOnURLs adds a server(s) based on the provided URL string.
// The URL can be a comma-separated list of URLs, URLs separated by new lines, or a single URL.
// Note that the UI allows the user to specify a server name. If there is only one URL, the server name overrides
// the tag typically included in the URL. If there are multiple URLs, the server name is ignored.
func (m *Manager) AddServerBasedOnURLs(ctx context.Context, urls string, skipCertVerification bool, serverName string) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "Manager.AddServerBasedOnURLs")
	defer span.End()
	urlProvider, loaded := pluriconfig.GetProvider(string(model.ProviderURL))
	if !loaded {
		return traces.RecordError(ctx, fmt.Errorf("URL config provider not loaded"))
	}
	cfg, err := urlProvider.Parse(ctx, []byte(urls))
	if err != nil {
		return traces.RecordError(ctx, fmt.Errorf("failed to parse URLs: %w", err))
	}
	cfgURLs, ok := cfg.Options.([]url.URL)
	if !ok || len(cfgURLs) == 0 {
		return traces.RecordError(ctx, fmt.Errorf("no valid URLs found in the provided configuration"))
	}

	// If we only have a single URL, and the server name is specified, use that
	// to override the tag specified in the anchor hash fragment.
	if len(cfgURLs) == 1 && serverName != "" {
		// override the tag, which is specified in the anchor hash fragment or
		// in the tag query parameter.
		q := cfgURLs[0].Query()
		q.Del("tag")
		cfgURLs[0].Fragment = serverName
		cfgURLs[0].RawQuery = q.Encode()
		cfg.Options = cfgURLs
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
	slog.Info("Adding servers based on URLs", "serverCount", len(cfgURLs), "skipCertVerification", skipCertVerification, "serverName", serverName)
	return m.AddServerWithSingboxJSON(ctx, singBoxCfg)
}
