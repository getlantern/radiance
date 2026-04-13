// Package vpn contains the peer proxy lifecycle for "Share My Connection."
// When enabled, the local Lantern client runs a samizdat inbound proxy on a
// UPnP-mapped port and registers with the API so censored users can connect.
package vpn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	sbox "github.com/sagernet/sing-box"
	sboxOption "github.com/sagernet/sing-box/option"

	box "github.com/getlantern/lantern-box"
	singjson "github.com/sagernet/sing/common/json"

	"github.com/getlantern/radiance/portforward"
)

// PeerProxy manages the lifecycle of a residential peer proxy: UPnP port
// mapping, API registration, sing-box server instance, and heartbeat.
type PeerProxy struct {
	mu sync.Mutex

	apiBase   string // e.g. "https://api.example.com/v1"
	deviceID  string
	userToken string

	forwarder  *portforward.Forwarder
	routeID    string
	instance   *sbox.Box
	cancelFunc context.CancelFunc

	active bool
}

// PeerProxyConfig holds the configuration needed to start a peer proxy.
type PeerProxyConfig struct {
	APIBase   string // API base URL (e.g. "https://api.getiantem.org/v1")
	DeviceID  string
	UserToken string
}

type peerRegisterRequest struct {
	ExternalIP   string `json:"external_ip"`
	ExternalPort uint16 `json:"external_port"`
	InternalPort uint16 `json:"internal_port"`
}

type peerRegisterResponse struct {
	RouteID                  string `json:"route_id"`
	ServerConfig             string `json:"server_config"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
}

// NewPeerProxy creates a new peer proxy manager.
func NewPeerProxy(cfg PeerProxyConfig) *PeerProxy {
	return &PeerProxy{
		apiBase:   cfg.APIBase,
		deviceID:  cfg.DeviceID,
		userToken: cfg.UserToken,
		forwarder: portforward.New(),
	}
}

// Start initiates the peer proxy: maps a port via UPnP, registers with the
// API, starts a sing-box instance with the returned server config, and begins
// heartbeating.
func (p *PeerProxy) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.active {
		return nil
	}

	slog.Info("Starting peer proxy")

	// 1. Pick a random internal port and map via UPnP
	internalPort := randomInternalPort()
	mapping, err := p.forwarder.MapPort(ctx, internalPort, "Lantern Peer Proxy")
	if err != nil {
		return fmt.Errorf("UPnP port mapping failed: %w", err)
	}
	slog.Info("UPnP port mapped",
		"internal_port", mapping.InternalPort,
		"external_port", mapping.ExternalPort,
		"method", mapping.Method,
	)

	// 2. Discover external IP
	externalIP, err := portforward.ExternalIP(ctx)
	if err != nil {
		_ = p.forwarder.UnmapPort(ctx)
		return fmt.Errorf("external IP discovery failed: %w", err)
	}
	slog.Info("External IP discovered", "ip", externalIP)

	// 3. Register with API — server generates credentials and returns sing-box config
	regResp, err := p.register(ctx, externalIP, mapping.ExternalPort, mapping.InternalPort)
	if err != nil {
		_ = p.forwarder.UnmapPort(ctx)
		return fmt.Errorf("peer registration failed: %w", err)
	}
	p.routeID = regResp.RouteID
	slog.Info("Peer proxy registered", "route_id", p.routeID)

	// 4. Start sing-box instance with the server config
	instance, err := p.startSingbox(regResp.ServerConfig)
	if err != nil {
		_ = p.deregister(ctx)
		_ = p.forwarder.UnmapPort(ctx)
		return fmt.Errorf("failed to start sing-box: %w", err)
	}
	p.instance = instance

	// 5. Start background goroutines
	peerCtx, cancel := context.WithCancel(ctx)
	p.cancelFunc = cancel

	// UPnP lease renewal
	p.forwarder.StartRenewal(peerCtx)

	// Heartbeat
	heartbeatInterval := time.Duration(regResp.HeartbeatIntervalSeconds) * time.Second
	if heartbeatInterval < time.Minute {
		heartbeatInterval = 5 * time.Minute
	}
	go p.heartbeatLoop(peerCtx, heartbeatInterval)

	p.active = true
	slog.Info("Peer proxy active",
		"external", fmt.Sprintf("%s:%d", externalIP, mapping.ExternalPort),
		"route_id", p.routeID,
	)
	return nil
}

// Stop shuts down the peer proxy: deregisters, stops sing-box, unmaps port.
func (p *PeerProxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.active {
		return nil
	}

	slog.Info("Stopping peer proxy", "route_id", p.routeID)

	// Cancel background goroutines
	if p.cancelFunc != nil {
		p.cancelFunc()
		p.cancelFunc = nil
	}

	// Deregister from API
	if err := p.deregister(ctx); err != nil {
		slog.Warn("Failed to deregister peer proxy", "error", err)
	}

	// Stop sing-box
	if p.instance != nil {
		if err := p.instance.Close(); err != nil {
			slog.Warn("Failed to close sing-box instance", "error", err)
		}
		p.instance = nil
	}

	// Unmap UPnP port
	if err := p.forwarder.UnmapPort(ctx); err != nil {
		slog.Warn("Failed to unmap UPnP port", "error", err)
	}

	p.routeID = ""
	p.active = false
	slog.Info("Peer proxy stopped")
	return nil
}

// Active returns true if the peer proxy is running.
func (p *PeerProxy) Active() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// register calls POST /v1/peer/register on the API.
func (p *PeerProxy) register(ctx context.Context, externalIP string, externalPort, internalPort uint16) (*peerRegisterResponse, error) {
	body, err := json.Marshal(peerRegisterRequest{
		ExternalIP:   externalIP,
		ExternalPort: externalPort,
		InternalPort: internalPort,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/peer/register", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lantern-Device-Id", p.deviceID)
	if p.userToken != "" {
		req.Header.Set("X-Lantern-Pro-Token", p.userToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("register failed: %s: %s", resp.Status, string(respBody))
	}

	var result peerRegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding register response: %w", err)
	}
	return &result, nil
}

// heartbeatLoop sends periodic heartbeats to keep the route alive.
func (p *PeerProxy) heartbeatLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := p.heartbeat(ctx); err != nil {
				slog.Warn("Peer proxy heartbeat failed", "error", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (p *PeerProxy) heartbeat(ctx context.Context) error {
	p.mu.Lock()
	routeID := p.routeID
	p.mu.Unlock()

	if routeID == "" {
		return nil
	}

	body, _ := json.Marshal(map[string]string{"route_id": routeID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/peer/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lantern-Device-Id", p.deviceID)
	if p.userToken != "" {
		req.Header.Set("X-Lantern-Pro-Token", p.userToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat: %s", resp.Status)
	}
	return nil
}

func (p *PeerProxy) deregister(ctx context.Context) error {
	if p.routeID == "" {
		return nil
	}

	body, _ := json.Marshal(map[string]string{"route_id": p.routeID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiBase+"/peer/deregister", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Lantern-Device-Id", p.deviceID)
	if p.userToken != "" {
		req.Header.Set("X-Lantern-Pro-Token", p.userToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deregister: %s", resp.Status)
	}
	return nil
}

// startSingbox creates and starts a sing-box instance with the given server config JSON.
func (p *PeerProxy) startSingbox(serverConfigJSON string) (*sbox.Box, error) {
	opts, err := singjson.UnmarshalExtendedContext[sboxOption.Options](box.BaseContext(), []byte(serverConfigJSON))
	if err != nil {
		return nil, fmt.Errorf("parsing server config: %w", err)
	}

	instance, err := sbox.New(sbox.Options{
		Context: box.BaseContext(),
		Options: opts,
	})
	if err != nil {
		return nil, fmt.Errorf("creating sing-box instance: %w", err)
	}

	if err := instance.Start(); err != nil {
		instance.Close()
		return nil, fmt.Errorf("starting sing-box: %w", err)
	}

	return instance, nil
}

func randomInternalPort() uint16 {
	// Use a port in the high range to avoid conflicts with common services
	return uint16(30000 + time.Now().UnixNano()%20000)
}
