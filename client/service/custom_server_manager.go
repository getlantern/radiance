package boxservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

type CustomServerManager struct {
	ctx                       context.Context
	customServersMutex        *sync.RWMutex
	customServers             map[string]CustomServerInfo
	customServersFilePath     string
	trustedServerFingerprints string
}

func NewCustomServerManager(ctx context.Context, dataDir string) *CustomServerManager {
	csm := &CustomServerManager{
		customServers:             make(map[string]CustomServerInfo),
		customServersMutex:        new(sync.RWMutex),
		customServersFilePath:     filepath.Join(dataDir, "custom_servers.json"),
		trustedServerFingerprints: filepath.Join(dataDir, "data", "trusted_server_fingerprints.json"),
	}
	csm.SetContext(ctx)
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

// SetContext update the context with the latest changes.
func (m *CustomServerManager) SetContext(ctx context.Context) {
	csm := service.PtrFromContext[CustomServerManager](ctx)
	if csm == nil {
		ctx = service.ContextWith(ctx, m)
	}
	m.ctx = ctx
}

// AddCustomServer load or parse the given configuration and add given
// endpdoint/outbound to the instance. We're only expecting one endpoint or
// outbound per call.
func (m *CustomServerManager) AddCustomServer(cfg ServerConnectConfig) error {
	loadedOptions, err := json.UnmarshalExtendedContext[CustomServerInfo](m.ctx, cfg)
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

	if err := updateOutboundsEndpoints(m.ctx, outbounds, endpoints); err != nil {
		return fmt.Errorf("failed to update outbounds/endpoints: %w", err)
	}

	if _, err := m.loadCustomServer(); err != nil {
		return fmt.Errorf("failed to load custom server configs: %w", err)
	}

	m.customServersMutex.Lock()
	m.customServers[tag] = loadedOptions
	m.customServersMutex.Unlock()
	if err := m.writeChanges(customServers{CustomServers: m.customServersMapToList(m.customServers)}); err != nil {
		return fmt.Errorf("failed to store custom server: %w", err)
	}

	if err := m.reinitializeCustomSelector("direct", []string{"direct", loadedOptions.Tag}); err != nil {
		return fmt.Errorf("failed to reinitialize custom selector: %w", err)
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

func (m *CustomServerManager) ListCustomServers() ([]CustomServerInfo, error) {
	return m.customServersMapToList(m.customServers), nil
}

func (m *CustomServerManager) writeChanges(customServers customServers) error {
	storedCustomServers, err := json.MarshalContext(m.ctx, customServers)
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

	if cs, err = json.UnmarshalExtendedContext[customServers](m.ctx, storedCustomServers); err != nil {
		return cs, fmt.Errorf("decode custom servers file: %w", err)
	}

	m.customServersMutex.Lock()
	defer m.customServersMutex.Unlock()
	for _, v := range cs.CustomServers {
		m.customServers[v.Tag] = v
	}

	return cs, nil
}

func (m *CustomServerManager) removeCustomServer(tag string) error {
	customServers, err := m.loadCustomServer()
	if err != nil {
		return fmt.Errorf("load custom servers: %w", err)
	}
	for i, server := range customServers.CustomServers {
		if server.Tag == tag {
			customServers.CustomServers = append(customServers.CustomServers[:i], customServers.CustomServers[i+1:]...)
			break
		}
	}
	if err = m.writeChanges(customServers); err != nil {
		return fmt.Errorf("failed to write custom server %q removal: %w", tag, err)
	}
	return nil
}

// RemoveCustomServer removes the custom server options from endpoints, outbounds
// and the custom server file.
func (m *CustomServerManager) RemoveCustomServer(tag string) error {
	if _, err := m.loadCustomServer(); err != nil {
		return fmt.Errorf("failed to load custom server configs: %w", err)
	}

	outboundManager := service.FromContext[adapter.OutboundManager](m.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](m.ctx)

	m.customServersMutex.RLock()
	options := m.customServers[tag]
	m.customServersMutex.RUnlock()

	if options.Outbound != nil {
		if _, exists := outboundManager.Outbound(options.Outbound.Tag); exists {
			// selector must be removed in order to remove dependent outbounds/endpoints
			if err := outboundManager.Remove(CustomSelectorTag); err != nil && !errors.Is(err, os.ErrInvalid) {
				return fmt.Errorf("failed to remove selector outbound: %w", err)
			}
			if err := outboundManager.Remove(options.Outbound.Tag); err != nil && !errors.Is(err, os.ErrInvalid) {
				return fmt.Errorf("failed to remove %q outbound: %w", tag, err)
			}
		}
	} else if options.Endpoint != nil {
		if _, exists := endpointManager.Get(options.Endpoint.Tag); exists {
			// selector must be removed in order to remove dependent outbounds/endpoints
			if err := outboundManager.Remove(CustomSelectorTag); err != nil && !errors.Is(err, os.ErrInvalid) {
				return fmt.Errorf("failed to remove selector outbound: %w", err)
			}
			if err := endpointManager.Remove(options.Endpoint.Tag); err != nil && !errors.Is(err, os.ErrInvalid) {
				return fmt.Errorf("failed to remove %q endpoint: %w", tag, err)
			}
		}
	}

	m.customServersMutex.Lock()
	delete(m.customServers, tag)
	m.customServersMutex.Unlock()
	if err := m.writeChanges(customServers{CustomServers: m.customServersMapToList(m.customServers)}); err != nil {
		return fmt.Errorf("failed to remove custom server %q: %w", tag, err)
	}

	if err := m.reinitializeCustomSelector("direct", []string{"direct"}); err != nil {
		return fmt.Errorf("failed to reinitialize custom selector: %w", err)
	}
	return nil
}

type selector interface {
	All() []string
	SelectOutbound(tag string) bool
	Now() string
}

// SelectCustomServer update the selector outbound to use the selected
// outbound based on provided tag. A selector outbound must exist before
// calling this function, otherwise it'll return a error.
func (m *CustomServerManager) SelectCustomServer(tag string) error {
	outboundManager := service.FromContext[adapter.OutboundManager](m.ctx)
	if _, exists := outboundManager.Outbound(tag); !exists {
		return fmt.Errorf("outbound %q not found", tag)
	}
	outbound, ok := outboundManager.Outbound(CustomSelectorTag)
	if !ok {
		return fmt.Errorf("custom selector not found")
	}
	selector, ok := outbound.(selector)
	if !ok {
		return fmt.Errorf("expected outbound that implements selector but got %T", outbound)
	}
	if ok = selector.SelectOutbound(tag); !ok {
		return fmt.Errorf("failed to select outbound %q", tag)
	}

	return nil
}

func (m *CustomServerManager) reinitializeCustomSelector(defaultTag string, tags []string) error {
	outboundManager := service.FromContext[adapter.OutboundManager](m.ctx)
	newTags := make([]string, 0)
	if outbound, exists := outboundManager.Outbound(CustomSelectorTag); exists {
		if selector, ok := outbound.(selector); ok {
			newTags = append(newTags, selector.All()...)
		}
		if err := outboundManager.Remove(CustomSelectorTag); err != nil {
			return fmt.Errorf("failed to remove selector outbound: %w", err)
		}
	}

	newTags = append(newTags, tags...)
	err := m.newSelectorOutbound(outboundManager, CustomSelectorTag, &option.SelectorOutboundOptions{
		Outbounds:                 newTags,
		Default:                   defaultTag,
		InterruptExistConnections: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create selector outbound: %w", err)
	}
	return nil
}

func (m *CustomServerManager) newSelectorOutbound(outboundManager adapter.OutboundManager, tag string, options *option.SelectorOutboundOptions) error {
	router := service.FromContext[adapter.Router](m.ctx)
	logFactory := service.FromContext[log.Factory](m.ctx)
	if err := outboundManager.Create(m.ctx, router, logFactory.NewLogger(tag), tag, constant.TypeSelector, options); err != nil {
		return fmt.Errorf("create selector outbound: %w", err)
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

	var cs connectInfo
	if cs, err = json.UnmarshalExtendedContext[connectInfo](m.ctx, body); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if len(cs.Outbounds) == 0 {
		return fmt.Errorf("no outbounds found")
	}

	cs.Outbounds[0].Tag = tag

	customServerConfig := CustomServerInfo{
		Outbound: cs.Outbounds[0],
	}
	data, err := json.MarshalContext(m.ctx, customServerConfig)
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
	if cs, err = json.UnmarshalExtendedContext[tokenResp](m.ctx, body); err != nil {
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
