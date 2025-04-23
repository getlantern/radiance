package boxservice

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/group"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
)

type CustomServerManager struct {
	ctx                   context.Context
	customServersMutex    *sync.RWMutex
	customServers         map[string]option.Options
	customServersFilePath string
}

func NewCustomServerManager(ctx context.Context, dataDir string) *CustomServerManager {
	return &CustomServerManager{
		ctx:                   ctx,
		customServers:         make(map[string]option.Options),
		customServersMutex:    new(sync.RWMutex),
		customServersFilePath: filepath.Join(dataDir, "data", "custom_servers.json"),
	}
}

// ServerConnectConfig represents configuration for connecting to a custom server.
type ServerConnectConfig []byte

// AddCustomServer load or parse the given configuration and add given
// endpdoint/outbound to the instance. We're only expecting one endpoint or
// outbound per call.
func (m *CustomServerManager) AddCustomServer(tag string, cfg ServerConnectConfig) error {
	m.customServersMutex.Lock()
	loadedOptions, configExist := m.customServers[tag]
	m.customServersMutex.Unlock()
	if configExist && cfg != nil {
		if err := m.RemoveCustomServer(tag); err != nil {
			return err
		}
	}

	if cfg != nil {
		var err error
		loadedOptions, err = json.UnmarshalExtendedContext[option.Options](m.ctx, cfg)
		if err != nil {
			return fmt.Errorf("failed to unmarshal config: %w", err)
		}
	}

	if err := updateOutboundsEndpoints(m.ctx, loadedOptions.Outbounds, loadedOptions.Endpoints); err != nil {
		return fmt.Errorf("failed to update outbounds/endpoints: %w", err)
	}

	m.customServersMutex.Lock()
	m.customServers[tag] = loadedOptions
	m.customServersMutex.Unlock()
	if err := m.storeCustomServer(tag, loadedOptions); err != nil {
		return fmt.Errorf("failed to store custom server: %w", err)
	}

	return nil
}

func (m *CustomServerManager) ListCustomServers() ([]CustomServerInfo, error) {
	loadedServers, err := m.loadCustomServer()
	if err != nil {
		return nil, fmt.Errorf("failed to load custom servers: %w", err)
	}

	return loadedServers.CustomServers, nil
}

// storeCustomServer stores the custom server configuration to a JSON file.
func (m *CustomServerManager) storeCustomServer(tag string, options option.Options) error {
	servers, err := m.loadCustomServer()
	if err != nil {
		return fmt.Errorf("load custom servers: %w", err)
	}

	if len(servers.CustomServers) == 0 {
		servers.CustomServers = make([]CustomServerInfo, 0)
		servers.CustomServers = append(servers.CustomServers, CustomServerInfo{
			Tag:     tag,
			Options: options,
		})
	} else {
		for i, server := range servers.CustomServers {
			if server.Tag == tag {
				server.Options = options
				servers.CustomServers[i] = server
				break
			}
		}
	}

	if err = m.writeChanges(servers); err != nil {
		return fmt.Errorf("failed to add custom server %q: %w", tag, err)
	}

	return nil
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

	if err := json.UnmarshalContext(m.ctx, storedCustomServers, &cs); err != nil {
		return cs, fmt.Errorf("decode custom servers file: %w", err)
	}

	m.customServersMutex.Lock()
	defer m.customServersMutex.Unlock()
	for _, v := range cs.CustomServers {
		m.customServers[v.Tag] = v.Options
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
	outboundManager := service.FromContext[adapter.OutboundManager](m.ctx)
	endpointManager := service.FromContext[adapter.EndpointManager](m.ctx)

	m.customServersMutex.Lock()
	options := m.customServers[tag]
	m.customServersMutex.Unlock()
	// selector must be removed in order to remove dependent outbounds
	if err := outboundManager.Remove(CustomSelectorTag); err != nil {
		return fmt.Errorf("failed to remove selector outbound: %w", err)
	}
	for _, outbounds := range options.Outbounds {
		if err := outboundManager.Remove(outbounds.Tag); err != nil && !errors.Is(err, os.ErrInvalid) {
			return fmt.Errorf("failed to remove %q outbound: %w", tag, err)
		}
	}

	for _, endpoints := range options.Endpoints {
		if err := endpointManager.Remove(endpoints.Tag); err != nil && !errors.Is(err, os.ErrInvalid) {
			return fmt.Errorf("failed to remove %q endpoint: %w", tag, err)
		}
	}

	m.customServersMutex.Lock()
	delete(m.customServers, tag)
	m.customServersMutex.Unlock()
	if err := m.removeCustomServer(tag); err != nil {
		return fmt.Errorf("failed to remove custom server %q: %w", tag, err)
	}
	return nil
}

// SelectCustomServer update the selector outbound to use the selected
// outbound based on provided tag. A selector outbound must exist before
// calling this function, otherwise it'll return a error.
func (m *CustomServerManager) SelectCustomServer(tag string) error {
	outboundManager := service.FromContext[adapter.OutboundManager](m.ctx)
	outbounds := outboundManager.Outbounds()
	tags := make([]string, 0)
	for _, outbound := range outbounds {
		// ignoring selector because it'll be removed and re-added with the new tags
		if outbound.Tag() == CustomSelectorTag {
			continue
		}
		tags = append(tags, outbound.Tag())
	}

	if _, exists := outboundManager.Outbound(CustomSelectorTag); exists {
		if err := outboundManager.Remove(CustomSelectorTag); err != nil {
			return fmt.Errorf("failed to remove selector outbound: %w", err)
		}
	}

	err := m.newSelectorOutbound(outboundManager, CustomSelectorTag, &option.SelectorOutboundOptions{
		Outbounds:                 tags,
		Default:                   tag,
		InterruptExistConnections: true,
	})
	if err != nil {
		return fmt.Errorf("failed to create selector outbound: %w", err)
	}

	outbound, ok := outboundManager.Outbound(CustomSelectorTag)
	if !ok {
		return fmt.Errorf("failed to get selector outbound: %w", err)
	}
	selector, ok := outbound.(*group.Selector)
	if !ok {
		return fmt.Errorf("expected outbound of type *group.Selector: %w", err)
	}
	if err = selector.Start(); err != nil {
		return fmt.Errorf("failed to start selector outbound: %w", err)
	}
	if ok = selector.SelectOutbound(tag); !ok {
		return fmt.Errorf("failed to select outbound %q: %w", tag, err)
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
