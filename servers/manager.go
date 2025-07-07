// Package servers provides management of user server configurations, including endpoints, outbounds,
// and trusted server fingerprints. It supports loading, and saving, as well as integration with
// remote server managers for adding, inviting, and revoking private servers with
// trust-on-first-use (TOFU) fingerprint verification.
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

	"github.com/getlantern/radiance/app"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

const (
	trustFingerprintFileName = "trusted_server_fingerprints.json"
)

// ServerOptions holds the configuration for user servers, including endpoints and outbounds.
type ServerOptions struct {
	Endpoints []option.Endpoint `json:"endpoints,omitempty"`
	Outbounds []option.Outbound `json:"outbounds,omitempty"`
}

// Manager manages user server configurations, including endpoints, outbounds, and trusted fingerprints.
type Manager struct {
	access     sync.RWMutex
	optsMap    map[string]any // Map of server tags to their options (Endpoint or Outbound).
	serverOpts ServerOptions

	serversFile      string
	fingerprintsFile string

	logger *slog.Logger
}

// NewManager creates a new Manager instance, loading server options from disk. If logger is nil,
// the default logger is used. Returns an error if loading servers fails.
func NewManager(dataPath string, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info("Initializing UserServerManager", "dataPath", dataPath)

	mgr := &Manager{
		optsMap: make(map[string]any),
		serverOpts: ServerOptions{
			Endpoints: make([]option.Endpoint, 0),
			Outbounds: make([]option.Outbound, 0),
		},
		serversFile:      filepath.Join(dataPath, app.UserServerFileName),
		fingerprintsFile: filepath.Join(dataPath, trustFingerprintFileName),
		logger:           logger,
	}

	if err := mgr.loadServers(); err != nil {
		return nil, fmt.Errorf("failed to load servers from file: %w", err)
	}

	return mgr, nil
}

// Servers returns the current [ServerOptions] managed by the Manager.
func (m *Manager) Servers() ServerOptions {
	return m.serverOpts
}

// GetServerByTag retrieves a server option by its tag. Returns the option and a boolean indicating
// if it was found.
func (m *Manager) GetServerByTag(tag string) (any, bool) {
	m.access.RLock()
	defer m.access.RUnlock()
	opts, ok := m.optsMap[tag]
	return opts, ok
}

// AddServerByConfig adds a server configuration from a JSON string. Returns an error if the config
// is invalid or the tag already exists.
func (m *Manager) AddServerByConfig(cfg string) error {
	// validate config
	tag, opts, err := m.unmarshalServerOpts([]byte(cfg))
	if err != nil {
		return fmt.Errorf("config must be a valid option.Endpoint or option.Outbound: %w", err)
	}
	m.access.Lock()
	defer m.access.Unlock()
	if _, exists := m.optsMap[tag]; exists {
		return fmt.Errorf("server with tag %q already exists", tag)
	}

	m.optsMap[tag] = opts
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers after adding %q: %w", tag, err)
	}
	return nil
}

// RemoveServer removes a server config by its tag.
func (m *Manager) RemoveServer(tag string) error {
	m.access.Lock()
	opts, exists := m.optsMap[tag]
	if !exists {
		m.access.Unlock()
		return nil
	}
	delete(m.optsMap, tag)
	switch v := opts.(type) {
	case option.Endpoint:
		m.serverOpts.Endpoints = remove(m.serverOpts.Endpoints, v)
	case option.Outbound:
		m.serverOpts.Outbounds = remove(m.serverOpts.Outbounds, v)
	}
	m.access.Unlock()
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

func (m *Manager) unmarshalServerOpts(cfg []byte) (string, any, error) {
	ctx := sbx.BoxContext()

	// first try to unmarshal the config as an Endpoint
	ep, err := json.UnmarshalExtendedContext[option.Endpoint](ctx, cfg)
	if err == nil { // config is a valid Endpoint
		return ep.Tag, ep, nil
	}
	// try to unmarshal it as an Outbound
	out, err := json.UnmarshalExtendedContext[option.Outbound](ctx, cfg)
	if err == nil { // config is a valid Outbound
		return out.Tag, out, nil
	}
	// if we reach here, the config is neither a valid an Endpoint nor an Outbound
	return "", nil, fmt.Errorf("failed to unmarshal config: %w", err)
}

func (m *Manager) saveServers() error {
	ctx := sbx.BoxContext()
	m.access.Lock()
	buf, err := json.MarshalContext(ctx, m.serverOpts)
	m.access.Unlock()
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
	opts, err := json.UnmarshalExtendedContext[ServerOptions](sbx.BoxContext(), buf)
	if err != nil {
		return fmt.Errorf("unmarshal server options: %w", err)
	}
	_ = m.merge(opts) // merge the loaded options into the manager
	return nil
}

func (m *Manager) addServers(opts ServerOptions) error {
	existingTags := m.merge(opts)
	if len(existingTags) > 0 {
		slog.Warn("Some servers were not added because they already exist", "tags", existingTags)
	}
	if err := m.saveServers(); err != nil {
		return fmt.Errorf("failed to save servers: %w", err)
	}
	if len(existingTags) > 0 {
		return fmt.Errorf("some servers were not added because they already exist: %v", existingTags)
	}
	return nil
}

// merge adds new endpoints and outbounds to the manager. If an endpoint or outbound with the same
// tag already exists, it will not be added again. merge returns the tags that were not added.
func (m *Manager) merge(opts ServerOptions) []string {
	if len(opts.Endpoints) == 0 && len(opts.Outbounds) == 0 {
		return nil
	}
	m.access.Lock()
	defer m.access.Unlock()
	var existingTags []string
	for _, ep := range opts.Endpoints {
		if _, exists := m.optsMap[ep.Tag]; !exists {
			m.optsMap[ep.Tag] = ep
			m.serverOpts.Endpoints = append(m.serverOpts.Endpoints, ep)
		} else {
			existingTags = append(existingTags, ep.Tag)
		}
	}
	for _, out := range opts.Outbounds {
		if _, exists := m.optsMap[out.Tag]; !exists {
			m.optsMap[out.Tag] = out
			m.serverOpts.Outbounds = append(m.serverOpts.Outbounds, out)
		} else {
			existingTags = append(existingTags, out.Tag)
		}
	}
	return existingTags
}

// Lantern Server Manager Integration

// AddPrivateServer fetches VPN connection info from a remote server manager and adds it as a server.
// Requires a trust fingerprint callback for certificate verification. If one isn't provided, it will
// prompt the user to trust the fingerprint.
func (m *Manager) AddPrivateServer(tag string, ip string, port int, accessToken string, trustFingerprintCallback TrustFingerprintCallback) error {
	if trustFingerprintCallback == nil {
		return fmt.Errorf("trustFingerprintCallback is required")
	}

	client, err := m.getClientForTrustedFingerprint(ip, port, trustFingerprintCallback)
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
	servers, err := json.UnmarshalExtendedContext[ServerOptions](ctx, body)
	if err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if len(servers.Endpoints) == 0 && len(servers.Outbounds) == 0 {
		return fmt.Errorf("no endpoints or outbounds in response")
	}

	// TODO: update when we support endpoints
	servers.Outbounds[0].Tag = tag // use the provided tag
	return m.addServers(servers)
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

func (m *Manager) getClientForTrustedFingerprint(ip string, port int, trustFingerprintCallback TrustFingerprintCallback) (*http.Client, error) {
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
