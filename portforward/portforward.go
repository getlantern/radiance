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
//
// Returns ErrNoPortForwarding only when discovery completes without finding
// a usable gateway. If ctx was canceled or its deadline expired during
// discovery, the ctx error is returned verbatim so callers can distinguish
// "this network can't host a peer" from "we ran out of time, retry later".
func NewForwarder(ctx context.Context) (*Forwarder, error) {
	if c, err := discoverIGDv2(ctx); err == nil && c != nil {
		return &Forwarder{client: c, method: "upnp-igd2"}, nil
	}
	if c, err := discoverIGDv1(ctx); err == nil && c != nil {
		return &Forwarder{client: c, method: "upnp-igd1"}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
		// Propagate ctx cancellation/deadline verbatim so callers can retry
		// rather than treating it as a permanent "this network won't work".
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("add port mapping: %w", ctxErr)
		}
		// Per the ErrNoPortForwarding docstring, a gateway refusing to map a
		// port is the "this network can't host a peer" case. Join the
		// sentinel so callers can detect it via errors.Is while still
		// surfacing the underlying router-specific reason for diagnostics.
		return nil, fmt.Errorf("add port mapping: %w", errors.Join(ErrNoPortForwarding, err))
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
//
// f.mapping is cleared only on a successful delete. A failed delete leaves
// the mapping in place so the caller can retry; otherwise we'd "forget"
// about a router rule that's actually still live and the user would have
// to wait for the UPnP lease to expire.
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
	err := runWithCtx(ctx, func() error {
		return client.DeletePortMapping("", m.ExternalPort, m.Protocol)
	})
	if err != nil {
		return fmt.Errorf("delete port mapping: %w", err)
	}
	f.mapping = nil
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

// localIP returns the LAN address the OS would use to reach the gateway.
//
// First tries the UDP-noop trick (let the kernel pick a route to a known
// public address) — fastest and most accurate when the host has a working
// default route. Falls back to scanning interfaces for a private IPv4 if
// that fails, which covers networks that block 8.8.8.8 outbound or use
// non-default IPv4 routing tables. UPnP IGD itself is IPv4 in IGDv1 and
// almost always IPv4 in IGDv2, so we only consider IPv4 addresses.
func localIP() (string, error) {
	if ip, err := localIPByDial(); err == nil {
		return ip, nil
	}
	return localIPByInterfaceScan()
}

func localIPByDial() (string, error) {
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

func localIPByInterfaceScan() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			return ip4.String(), nil
		}
	}
	return "", fmt.Errorf("no usable private ipv4 found on any interface")
}

func LocalIP() (string, error) { return localIP() }

// runWithCtx wraps a blocking call so the caller's context can abort the
// wait. Returns ctx.Err() immediately if ctx is already cancelled, so the
// gateway-side side effect (port mapping, etc.) doesn't fire after the
// caller has already given up. If ctx cancels mid-call, the wrapped
// goroutine still runs to completion — UPnP/HTTP calls have their own
// underlying timeouts — but we no longer hand the entire wait time to an
// unresponsive gateway.
func runWithCtx(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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

// discoverIGDv1 looks for both WANIPConnection and WANPPPConnection gateways.
// Cable/fiber CPE routers typically expose UPnP via WANIPConnection; DSL and
// other PPPoE-terminated CPEs typically expose it via WANPPPConnection.
// Probing only one would miss large swaths of consumer hardware.
func discoverIGDv1(ctx context.Context) (igdClient, error) {
	if clients, _, err := internetgateway1.NewWANIPConnection1ClientsCtx(ctx); err == nil && len(clients) > 0 {
		return wanIPv1Wrapper{c: clients[0]}, nil
	}
	clients, _, err := internetgateway1.NewWANPPPConnection1ClientsCtx(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, nil
	}
	return wanPPPv1Wrapper{c: clients[0]}, nil
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

type wanPPPv1Wrapper struct{ c *internetgateway1.WANPPPConnection1 }

func (w wanPPPv1Wrapper) AddPortMapping(remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	return w.c.AddPortMapping(remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration)
}
func (w wanPPPv1Wrapper) DeletePortMapping(remoteHost string, externalPort uint16, protocol string) error {
	return w.c.DeletePortMapping(remoteHost, externalPort, protocol)
}
func (w wanPPPv1Wrapper) GetExternalIPAddress() (string, error) {
	return w.c.GetExternalIPAddress()
}

var (
	_ igdClient = wanIPv2Wrapper{}
	_ igdClient = wanIPv1Wrapper{}
	_ igdClient = wanPPPv1Wrapper{}
)
