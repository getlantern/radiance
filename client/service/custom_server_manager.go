package boxservice

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	sbx "github.com/getlantern/sing-box-extensions"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
)

type CustomServerManager struct {
	customServersMutex        sync.RWMutex
	customServers             map[string]CustomServerInfo
	customServersFilePath     string
	trustedServerFingerprints string
}

func NewCustomServerManager(dataDir string) *CustomServerManager {
	csm := &CustomServerManager{
		customServers:             make(map[string]CustomServerInfo),
		customServersFilePath:     filepath.Join(dataDir, "custom_servers.json"),
		trustedServerFingerprints: filepath.Join(dataDir, "trusted_server_fingerprints.json"),
	}
	return csm
}

type customServers struct {
	CustomServers []CustomServerInfo `json:"custom_servers"`
}

// CustomServerInfo represents a custom server configuration.
// Outbound and Endpoint options are mutually exclusive and there can only be
// one of those fields non-nil.
type CustomServerInfo struct {
	Tag      string           `json:"tag"`
	Outbound *option.Outbound `json:"outbound,omitempty"`
	Endpoint *option.Endpoint `json:"endpoint,omitempty"`
}

// ServerConnectConfig represents configuration for connecting to a custom server.
type ServerConnectConfig []byte

// AddCustomServer load or parse the given configuration and add given
// endpdoint/outbound to the instance. We're only expecting one endpoint or
// outbound per call.
func (m *CustomServerManager) AddCustomServer(cfg ServerConnectConfig) error {
	// validate config
	ctx := sbx.BoxContext()
	loadedOptions, err := json.UnmarshalExtendedContext[CustomServerInfo](ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if loadedOptions.Endpoint == nil && loadedOptions.Outbound == nil {
		return fmt.Errorf("invalid custom server provided")
	}

	outbounds := make([]option.Outbound, 0)
	endpoints := make([]option.Endpoint, 0)
	var tag string
	if loadedOptions.Outbound != nil {
		outbounds = append(outbounds, *loadedOptions.Outbound)
		tag = loadedOptions.Outbound.Tag
	} else if loadedOptions.Endpoint != nil {
		endpoints = append(endpoints, *loadedOptions.Endpoint)
		tag = loadedOptions.Endpoint.Tag
	}
	loadedOptions.Tag = tag

	if _, err := m.loadCustomServer(); err != nil {
		return fmt.Errorf("failed to load custom server configs: %w", err)
	}

	m.customServersMutex.Lock()
	m.customServers[tag] = loadedOptions
	m.customServersMutex.Unlock()
	if err := m.writeChanges(customServers{CustomServers: m.customServersMapToList(m.customServers)}); err != nil {
		return fmt.Errorf("failed to store custom server: %w", err)
	}

	return nil
}

func (m *CustomServerManager) customServersMapToList(a map[string]CustomServerInfo) []CustomServerInfo {
	m.customServersMutex.RLock()
	defer m.customServersMutex.RUnlock()
	customServers := make([]CustomServerInfo, 0)
	for _, v := range a {
		customServers = append(customServers, v)
	}
	return customServers
}

func (m *CustomServerManager) ListCustomServers() []CustomServerInfo {
	return m.customServersMapToList(m.customServers)
}

func (m *CustomServerManager) GetServerByTag(tag string) (any, bool) {
	m.customServersMutex.RLock()
	defer m.customServersMutex.RUnlock()
	server, ok := m.customServers[tag]
	if !ok {
		return nil, false
	}
	if server.Outbound != nil {
		return server.Outbound, true
	}
	return server.Endpoint, true
}

func (m *CustomServerManager) writeChanges(customServers customServers) error {
	ctx := sbx.BoxContext()
	storedCustomServers, err := json.MarshalContext(ctx, customServers)
	if err != nil {
		return fmt.Errorf("marshal custom servers: %w", err)
	}
	if err := os.WriteFile(m.customServersFilePath, storedCustomServers, 0644); err != nil {
		return fmt.Errorf("write custom servers file: %w", err)
	}
	return nil
}

// loadCustomServer loads the custom server configuration from a JSON file.
func (m *CustomServerManager) loadCustomServer() (customServers, error) {
	var cs customServers
	if err := os.MkdirAll(filepath.Dir(m.customServersFilePath), 0755); err != nil {
		return cs, err
	}
	// read file and generate []byte
	storedCustomServers, err := os.ReadFile(m.customServersFilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// file not exist, return empty custom servers
			return cs, nil
		}
		return cs, fmt.Errorf("read custom servers file: %w", err)
	}

	ctx := sbx.BoxContext()
	if cs, err = json.UnmarshalExtendedContext[customServers](ctx, storedCustomServers); err != nil {
		return cs, fmt.Errorf("decode custom servers file: %w", err)
	}

	m.customServersMutex.Lock()
	defer m.customServersMutex.Unlock()
	for _, v := range cs.CustomServers {
		m.customServers[v.Tag] = v
	}

	return cs, nil
}

// RemoveCustomServer removes the custom server options from endpoints, outbounds
// and the custom server file.
func (m *CustomServerManager) RemoveCustomServer(tag string) error {
	if _, err := m.loadCustomServer(); err != nil {
		return fmt.Errorf("failed to load custom server configs: %w", err)
	}

	m.customServersMutex.Lock()
	delete(m.customServers, tag)
	m.customServersMutex.Unlock()

	if err := m.writeChanges(customServers{CustomServers: m.customServersMapToList(m.customServers)}); err != nil {
		return fmt.Errorf("failed to remove custom server %q: %w", tag, err)
	}
	return nil
}

func (m *CustomServerManager) getClientForTrustedFingerprint(ip string, port int, trustFingerprintCallback TrustFingerprintCallback) (*http.Client, error) {
	// get server fingerprints via TLS
	details, err := getServerFingerprints(ip, port)
	if err != nil {
		return nil, fmt.Errorf("failed to get server fingerprints: %w", err)
	}
	// check if we already have the trusted fingerprint
	fingerprints, trustedFingerprint, err := getTrustedServerFingerprint(m.trustedServerFingerprints, ip, details)
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
			if err := writeTrustedServerFingerprints(m.trustedServerFingerprints, fingerprints); err != nil {
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

func (m *CustomServerManager) AddServerManagerInstance(tag string, ip string, port int, accessToken string, trustFingerprintCallback TrustFingerprintCallback) error {
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
	type connectInfo struct {
		Outbounds []*option.Outbound `json:"outbounds,omitempty"`
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	defer resp.Body.Close()

	ctx := sbx.BoxContext()
	var cs connectInfo
	if cs, err = json.UnmarshalExtendedContext[connectInfo](ctx, body); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if len(cs.Outbounds) == 0 {
		return fmt.Errorf("no outbounds found")
	}

	cs.Outbounds[0].Tag = tag

	customServerConfig := CustomServerInfo{
		Outbound: cs.Outbounds[0],
	}
	data, err := json.MarshalContext(ctx, customServerConfig)
	if err != nil {
		return fmt.Errorf("marshal custom server config: %w", err)
	}
	return m.AddCustomServer(data)
}

func (m *CustomServerManager) InviteToServerManagerInstance(ip string, port int, accessToken string, inviteName string) (string, error) {
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

func (m *CustomServerManager) RevokeServerManagerInvite(ip string, port int, accessToken string, inviteName string) error {
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
