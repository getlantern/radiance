// Package portforward opens TCP ports on the local network gateway via UPnP
// IGD so a peer-proxy inbound is reachable from the public internet without
// manual router configuration. IGDv2 is tried first and IGDv1 is the
// fallback.
package portforward

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway1"
	"github.com/huin/goupnp/dcps/internetgateway2"
)

// ErrNoPortForwarding is returned when no UPnP gateway is reachable, the
// gateway refuses to map a port, or the discovery scan times out. Callers
// should treat this as "this network can't host a peer proxy" and surface it
// to the user rather than retry indefinitely.
var ErrNoPortForwarding = errors.New("no port forwarding available")

type Mapping struct {
	ExternalPort  uint16
	InternalPort  uint16
	InternalIP    string
	Protocol      string
	LeaseDuration time.Duration
	Method        string
}

// igdClient is the subset of the IGDv2/v1 clients we use. goupnp's generated
// clients already satisfy this shape.
type igdClient interface {
	AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddress() (string, error)
}

// Forwarder manages a single port mapping on the local gateway. Construct
// one per peer-proxy session.
type Forwarder struct {
	mu      sync.Mutex
	client  igdClient
	method  string
	mapping *Mapping
	cancel  context.CancelFunc
}

// NewForwarder discovers the local gateway and returns a Forwarder bound to
// it. Callers should pick a 5-10s timeout on ctx — UPnP discovery is M-SEARCH
// multicast and waits for replies.
func NewForwarder(ctx context.Context) (*Forwarder, error) {
	if c, err := discoverIGDv2(ctx); err == nil && c != nil {
		return &Forwarder{client: c, method: "upnp-igd2"}, nil
	}
	if c, err := discoverIGDv1(ctx); err == nil && c != nil {
		return &Forwarder{client: c, method: "upnp-igd1"}, nil
	}
	return nil, ErrNoPortForwarding
}

// MapPort asks the gateway to forward externalPort → (LocalIP():internalPort)
// for TCP. Lease duration is requested as 1 hour but some routers ignore the
// request and assign their own (or none — "permanent"). description is shown
// in the router's UI so users can identify and remove the mapping manually
// if needed.
func (f *Forwarder) MapPort(ctx context.Context, internalPort uint16, description string) (*Mapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mapping != nil {
		return nil, errors.New("forwarder already has an active mapping")
	}

	internalIP, err := localIP()
	if err != nil {
		return nil, fmt.Errorf("determine local ip: %w", err)
	}

	const requestedLease uint32 = 3600
	// externalPort defaults to internalPort. If the router already has that
	// port mapped to someone else, AddPortMapping fails and the caller can
	// retry with a different internalPort.
	externalPort := internalPort
	client := f.client
	err = runWithCtx(ctx, func() error {
		return client.AddPortMapping("", externalPort, "TCP", internalPort, internalIP, true, description, requestedLease)
	})
	if err != nil {
		return nil, fmt.Errorf("add port mapping: %w", err)
	}

	f.mapping = &Mapping{
		ExternalPort:  externalPort,
		InternalPort:  internalPort,
		InternalIP:    internalIP,
		Protocol:      "TCP",
		LeaseDuration: time.Duration(requestedLease) * time.Second,
		Method:        f.method,
	}
	return f.mapping, nil
}

// UnmapPort removes the active mapping. No-op if no mapping is active.
// Always called as part of teardown — even if the gateway has already let
// the lease expire, DeletePortMapping is the polite signal to the router.
func (f *Forwarder) UnmapPort(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if f.mapping == nil {
		return nil
	}
	m := f.mapping
	client := f.client
	f.mapping = nil
	err := runWithCtx(ctx, func() error {
		return client.DeletePortMapping("", m.ExternalPort, m.Protocol)
	})
	if err != nil {
		return fmt.Errorf("delete port mapping: %w", err)
	}
	return nil
}

// StartRenewal launches a goroutine that re-issues AddPortMapping at half
// the lease duration (minimum 1 minute) until ctx is cancelled or UnmapPort
// is called. Routers that ignored the requested lease and assigned their
// own short TTL would otherwise drop the mapping mid-session.
func (f *Forwarder) StartRenewal(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		return
	}
	if f.mapping == nil {
		return
	}
	renewCtx, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	interval := f.mapping.LeaseDuration / 2
	if interval < 1*time.Minute {
		interval = 1 * time.Minute
	}
	go f.renewLoop(renewCtx, interval)
}

func (f *Forwarder) renewLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.mu.Lock()
			m := f.mapping
			client := f.client
			f.mu.Unlock()
			if m == nil {
				return
			}
			// Most routers treat a re-issued AddPortMapping as "extend the
			// existing lease"; some replace it with a fresh one. Either is
			// fine here.
			err := runWithCtx(ctx, func() error {
				return client.AddPortMapping("", m.ExternalPort, "TCP", m.InternalPort, m.InternalIP, true, "Lantern peer share (renew)", uint32(m.LeaseDuration/time.Second))
			})
			if err != nil {
				slog.Warn("portforward: lease renewal failed", "err", err, "external_port", m.ExternalPort)
			}
		}
	}
}

// ExternalIP queries the gateway for its WAN-side IP address. Cheaper than
// dialing a public-IP service when we already have a UPnP client open.
func (f *Forwarder) ExternalIP(ctx context.Context) (string, error) {
	f.mu.Lock()
	c := f.client
	f.mu.Unlock()
	var ip string
	err := runWithCtx(ctx, func() error {
		got, gerr := c.GetExternalIPAddress()
		if gerr != nil {
			return gerr
		}
		ip = got
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("get external ip: %w", err)
	}
	if ip == "" {
		return "", fmt.Errorf("gateway returned empty external ip")
	}
	return ip, nil
}

// localIP dials an external UDP "no-op" address and inspects the source IP
// the OS would have chosen — no packets are actually sent.
func localIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "", fmt.Errorf("dial udp for local ip: %w", err)
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return "", fmt.Errorf("unexpected local addr type %T", conn.LocalAddr())
	}
	return addr.IP.String(), nil
}

func LocalIP() (string, error) { return localIP() }

// runWithCtx wraps a blocking call so the caller's context can abort the
// wait. The wrapped goroutine still runs to completion and may leak briefly
// — UPnP/HTTP calls have their own underlying timeouts — but we no longer
// hand the entire wait time to an unresponsive gateway.
func runWithCtx(ctx context.Context, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func discoverIGDv2(ctx context.Context) (igdClient, error) {
	clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	return wanIPv2Wrapper{c: clients[0]}, nil
}

func discoverIGDv1(ctx context.Context) (igdClient, error) {
	clients, _, err := internetgateway1.NewWANIPConnection1ClientsCtx(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	return wanIPv1Wrapper{c: clients[0]}, nil
}

// IGDv1 and IGDv2's generated clients have slightly different method
// signatures, so wrappers normalize them to a single igdClient interface.

type wanIPv2Wrapper struct{ c *internetgateway2.WANIPConnection2 }

func (w wanIPv2Wrapper) AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return w.c.AddPortMapping(remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration)
}
func (w wanIPv2Wrapper) DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error {
	return w.c.DeletePortMapping(remoteHost, externalPort, protocol)
}
func (w wanIPv2Wrapper) GetExternalIPAddress() (string, error) {
	return w.c.GetExternalIPAddress()
}

type wanIPv1Wrapper struct{ c *internetgateway1.WANIPConnection1 }

func (w wanIPv1Wrapper) AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return w.c.AddPortMapping(remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration)
}
func (w wanIPv1Wrapper) DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error {
	return w.c.DeletePortMapping(remoteHost, externalPort, protocol)
}
func (w wanIPv1Wrapper) GetExternalIPAddress() (string, error) {
	return w.c.GetExternalIPAddress()
}

var (
	_ igdClient = wanIPv2Wrapper{}
	_ igdClient = wanIPv1Wrapper{}
)
