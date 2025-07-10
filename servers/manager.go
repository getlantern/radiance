// Package servers provides management of server configurations, such as option.Endpoints and
// option.Outbounds,and integration with remote server managers for adding, inviting, and revoking
// private servers with trust-on-first-use (TOFU) fingerprint verification.
package servers

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	sbx "github.com/getlantern/sing-box-extensions"

	C "github.com/getlantern/common"

	"github.com/getlantern/radiance/common"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

type ServerGroup = string

const (
	SGLantern ServerGroup = "lantern"
	SGUser    ServerGroup = "user"

	trustFingerprintFileName = "trusted_server_fingerprints.json"
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

	serversFile      string
	fingerprintsFile string

	log *slog.Logger
}

// NewManager creates a new Manager instance, loading server options from disk.
func NewManager(dataPath string, logger *slog.Logger) (*Manager, error) {
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
		serversFile:      filepath.Join(dataPath, common.ServersFileName),
		fingerprintsFile: filepath.Join(dataPath, trustFingerprintFileName),
		log:              logger,
		access:           sync.RWMutex{},
	}

	if err := mgr.loadServers(); err != nil {
		return nil, fmt.Errorf("failed to load servers from file: %w", err)
	}

	return mgr, nil
}

// Servers returns the current server configurations for both groups ([SGLantern] and [SGUser]).
func (m *Manager) Servers() Servers {
	return m.servers
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
		Location: m.servers[SGLantern].Locations[tag],
	}
	switch v := opts.(type) {
	case option.Endpoint:
		s.Type = v.Type
	case option.Outbound:
		s.Type = v.Type
	}
	return s, true
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

	m.servers[group] = options
	opts := make(map[string]any)
	for _, ep := range options.Endpoints {
		opts[ep.Tag] = ep
	}
	for _, out := range options.Outbounds {
		opts[out.Tag] = out
	}
	m.optsMaps[group] = opts
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
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

	existingTags := m.merge(group, opts)
	if len(existingTags) > 0 {
		m.log.Warn("Some servers were not added because they already exist", "tags", existingTags)
	}
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
	if len(existingTags) > 0 {
		return fmt.Errorf("some servers were not added because they already exist: %v", existingTags)
	}
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

	// check which group the server belongs to so we can get the correct optsMaps and servers
	opts := m.optsMaps[SGLantern]
	servers := m.servers[SGLantern]
	if _, exists := opts[tag]; !exists {
		opts = m.optsMaps[SGUser]
		servers = m.servers[SGUser]
		if _, exists := opts[tag]; !exists {
			return fmt.Errorf("server with tag %q not found", tag)
		}
	}
	// remove the server from the optsMaps and servers
	switch v := opts[tag].(type) {
	case option.Endpoint:
		servers.Endpoints = remove(servers.Endpoints, v)
	case option.Outbound:
		servers.Outbounds = remove(servers.Outbounds, v)
	}
	delete(opts, tag)
	delete(servers.Locations, tag)
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers after removing %q: %w", tag, err)
	}
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
	ctx := sbx.BoxContext()
	buf, err := json.MarshalContext(ctx, m.servers)
	if err != nil {
		return fmt.Errorf("marshal servers: %w", err)
	}
	return os.WriteFile(m.serversFile, buf, 0600)
}

func (m *Manager) loadServers() error {
	buf, err := os.ReadFile(m.serversFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil // file doesn't exist
	}
	if err != nil {
		return fmt.Errorf("read server file %q: %w", m.serversFile, err)
	}
	servers, err := json.UnmarshalExtendedContext[Servers](sbx.BoxContext(), buf)
	if err != nil {
		return fmt.Errorf("unmarshal server options: %w", err)
	}
	m.SetServers(SGLantern, servers[SGLantern])
	m.SetServers(SGUser, servers[SGUser])
	return nil
}

// Lantern Server Manager Integration

// AddPrivateServer fetches VPN connection info from a remote server manager and adds it as a server.
// Requires a trust fingerprint callback for certificate verification. If one isn't provided, it will
// prompt the user to trust the fingerprint.
func (m *Manager) AddPrivateServer(tag string, ip string, port int, accessToken string, trustFingerprintCB TrustFingerprintCB) error {
	if trustFingerprintCB == nil {
		return fmt.Errorf("trustFingerprintCB is required")
	}

	client, err := m.getClientForTrustedFingerprint(ip, port, trustFingerprintCB)
	if err != nil {
		return err
	}

	resp, err := client.Get(fmt.Sprintf("https://%s:%d/api/v1/connect-config?token=%s", ip, port, accessToken))
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to get connect config: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	defer resp.Body.Close()

	ctx := sbx.BoxContext()
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
// token. The server must be added to the user's servers first and have a trusted fingerprint.
func (m *Manager) InviteToPrivateServer(ip string, port int, accessToken string, inviteName string) (string, error) {
	client, err := m.getClientForTrustedFingerprint(ip, port, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Get(fmt.Sprintf("https://%s:%d/api/v1/share-link/%s?token=%s", ip, port, inviteName, accessToken))
	if err != nil {
		return "", fmt.Errorf("failed to send request: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("failed to get connect config: %w", err)
	}
	type tokenResp struct {
		Token string
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	defer resp.Body.Close()

	var cs tokenResp
	if err = json.Unmarshal(body, &cs); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	return cs.Token, nil
}

// RevokePrivateServerInvite will revoke an invite to the server manager instance. The server must
// be added to the user's servers first and have a trusted fingerprint.
func (m *Manager) RevokePrivateServerInvite(ip string, port int, accessToken string, inviteName string) error {
	client, err := m.getClientForTrustedFingerprint(ip, port, nil)
	if err != nil {
		return err
	}

	resp, err := client.Post(fmt.Sprintf("https://%s:%d/api/v1/revoke/%s?token=%s", ip, port, inviteName, accessToken), "application/json", nil)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to revoke invite: %w", err)
	}
	return nil
}

func (m *Manager) getClientForTrustedFingerprint(ip string, port int, trustFingerprintCallback TrustFingerprintCB) (*http.Client, error) {
	// get server fingerprints via TLS
	details, err := getServerFingerprints(ip, port)
	if err != nil {
		return nil, fmt.Errorf("failed to get server fingerprints: %w", err)
	}
	// check if we already have the trusted fingerprint
	fingerprints, trustedFingerprint, err := getTrustedServerFingerprint(m.fingerprintsFile, ip, details)
	if err != nil {
		return nil, fmt.Errorf("failed to get trusted server fingerprint: %w", err)
	}
	// if not - attempt to ask the user to select a fingerprint
	if trustedFingerprint == "" && trustFingerprintCallback != nil {
		if ct := trustFingerprintCallback(ip, details); ct == nil {
			return nil, ErrTrustCancelled
		} else {
			// user accepted the fingerprint. save it
			fingerprints[ip] = ct.Fingerprint
			if err := writeTrustedServerFingerprints(m.fingerprintsFile, fingerprints); err != nil {
				return nil, fmt.Errorf("failed to write trusted server fingerprints: %w", err)
			}
			trustedFingerprint = ct.Fingerprint
		}
	}
	// assemble an http client with the trusted fingerprint
	client, err := getTOFUClient(trustedFingerprint)
	if err != nil {
		return nil, fmt.Errorf("failed to get tofu client: %w", err)
	}
	return client, nil
}
