// Package portforward opens ports on the local network gateway via UPnP IGD.
// It is used by the "Share My Connection" feature to make a peer proxy
// reachable from the internet.
package portforward

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway2"
)

// ErrNoPortForwarding is returned when no port forwarding method (UPnP, NAT-PMP)
// is available on the network.
var ErrNoPortForwarding = errors.New("no port forwarding method available")

// Mapping holds the details of an active port mapping.
type Mapping struct {
	ExternalPort  uint16
	InternalPort  uint16
	InternalIP    string
	Protocol      string // "TCP" or "UDP"
	LeaseDuration time.Duration
	Method        string // "upnp-igd2", "upnp-igd1"
}

// Forwarder manages port mappings on the local gateway via UPnP IGD.
type Forwarder struct {
	mu      sync.Mutex
	mapping *Mapping
	stopC   chan struct{}

	igd2Client *internetgateway2.WANIPConnection2
	igd1Client *internetgateway2.WANIPConnection1
}

// New creates a Forwarder. Call MapPort to actually open a port.
func New() *Forwarder {
	return &Forwarder{}
}

// MapPort opens a port on the gateway, forwarding external traffic to the given
// internal port on this machine. It tries UPnP IGDv2, then IGDv1. If the
// requested external port is taken, it retries with random ports.
func (f *Forwarder) MapPort(ctx context.Context, internalPort uint16, description string) (*Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.mapping != nil {
		return f.mapping, nil
	}

	localIP, err := LocalIP()
	if err != nil {
		return nil, fmt.Errorf("determine local IP: %w", err)
	}

	// Try IGDv2 first
	if m, err := f.tryIGD2(ctx, localIP, internalPort, description); err == nil {
		f.mapping = m
		return m, nil
	}

	// Fall back to IGDv1
	if m, err := f.tryIGD1(ctx, localIP, internalPort, description); err == nil {
		f.mapping = m
		return m, nil
	}

	return nil, fmt.Errorf("%w: UPnP IGD not available or port mapping denied", ErrNoPortForwarding)
}

// UnmapPort removes the active port mapping from the gateway.
func (f *Forwarder) UnmapPort(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.mapping == nil {
		return nil
	}

	f.stopRenewal()

	var err error
	switch {
	case f.igd2Client != nil:
		err = f.igd2Client.DeletePortMappingCtx(ctx, "", f.mapping.ExternalPort, f.mapping.Protocol)
	case f.igd1Client != nil:
		err = f.igd1Client.DeletePortMappingCtx(ctx, "", f.mapping.ExternalPort, f.mapping.Protocol)
	}

	f.mapping = nil
	f.igd2Client = nil
	f.igd1Client = nil
	return err
}

// Active returns the current active mapping, or nil if none.
func (f *Forwarder) Active() *Mapping {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mapping
}

// StartRenewal begins a background goroutine that refreshes the port mapping
// at half the lease interval. Stopped automatically by UnmapPort.
func (f *Forwarder) StartRenewal(ctx context.Context) {
	f.mu.Lock()
	if f.mapping == nil || f.stopC != nil {
		f.mu.Unlock()
		return
	}
	f.stopC = make(chan struct{})
	m := *f.mapping
	pf := f // capture for goroutine
	f.mu.Unlock()

	renewInterval := m.LeaseDuration / 2
	if renewInterval < time.Minute {
		renewInterval = time.Minute
	}

	go func() {
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pf.renewMapping(ctx, m)
			case <-pf.stopC:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (f *Forwarder) renewMapping(ctx context.Context, m Mapping) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mapping == nil {
		return
	}
	switch {
	case f.igd2Client != nil:
		_ = f.igd2Client.AddPortMappingCtx(ctx,
			"", m.ExternalPort, m.Protocol,
			m.InternalPort, m.InternalIP,
			true, "Lantern Peer Proxy",
			uint32(m.LeaseDuration.Seconds()),
		)
	case f.igd1Client != nil:
		_ = f.igd1Client.AddPortMappingCtx(ctx,
			"", m.ExternalPort, m.Protocol,
			m.InternalPort, m.InternalIP,
			true, "Lantern Peer Proxy",
			uint32(m.LeaseDuration.Seconds()),
		)
	}
}

func (f *Forwarder) stopRenewal() {
	if f.stopC != nil {
		close(f.stopC)
		f.stopC = nil
	}
}

const (
	maxRetries    = 10
	leaseDuration = 3600 // 1 hour in seconds
	portRangeMin  = 10000
	portRangeMax  = 60000
)

func (f *Forwarder) tryIGD2(ctx context.Context, localIP string, internalPort uint16, desc string) (*Mapping, error) {
	clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("no IGDv2 gateway found")
	}
	client := clients[0]

	for i := 0; i < maxRetries; i++ {
		extPort := randomPort()
		err = client.AddPortMappingCtx(ctx,
			"", extPort, "TCP",
			internalPort, localIP,
			true, desc, leaseDuration,
		)
		if err == nil {
			f.igd2Client = client
			return &Mapping{
				ExternalPort:  extPort,
				InternalPort:  internalPort,
				InternalIP:    localIP,
				Protocol:      "TCP",
				LeaseDuration: time.Duration(leaseDuration) * time.Second,
				Method:        "upnp-igd2",
			}, nil
		}
	}
	return nil, fmt.Errorf("IGDv2 failed after %d attempts: %w", maxRetries, err)
}

func (f *Forwarder) tryIGD1(ctx context.Context, localIP string, internalPort uint16, desc string) (*Mapping, error) {
	clients, _, err := internetgateway2.NewWANIPConnection1ClientsCtx(ctx)
	if err != nil || len(clients) == 0 {
		return nil, fmt.Errorf("no IGDv1 gateway found")
	}
	client := clients[0]

	for i := 0; i < maxRetries; i++ {
		extPort := randomPort()
		err = client.AddPortMappingCtx(ctx,
			"", extPort, "TCP",
			internalPort, localIP,
			true, desc, leaseDuration,
		)
		if err == nil {
			f.igd1Client = client
			return &Mapping{
				ExternalPort:  extPort,
				InternalPort:  internalPort,
				InternalIP:    localIP,
				Protocol:      "TCP",
				LeaseDuration: time.Duration(leaseDuration) * time.Second,
				Method:        "upnp-igd1",
			}, nil
		}
	}
	return nil, fmt.Errorf("IGDv1 failed after %d attempts: %w", maxRetries, err)
}

func randomPort() uint16 {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(portRangeMax-portRangeMin)))
	return uint16(n.Int64()) + portRangeMin
}

// LocalIP determines this machine's LAN IP by dialing a UDP socket to a
// well-known address. No actual traffic is sent.
func LocalIP() (string, error) {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// ExternalIP discovers this machine's public IP by querying external services.
// It tries multiple providers for redundancy.
func ExternalIP(ctx context.Context) (string, error) {
	providers := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
		"https://icanhazip.com",
	}
	for _, url := range providers {
		ip, err := fetchExternalIP(ctx, url)
		if err == nil && ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("failed to determine external IP from any provider")
}

func fetchExternalIP(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("invalid IP: %q", ip)
	}
	return ip, nil
}
