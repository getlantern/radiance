package portforward

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/getlantern/publicip"
)

// ManualForwarder is a no-op port forwarder for users who can't (or won't)
// use UPnP and have manually configured a port forward on their router.
// Assumes a 1:1 router mapping (WAN:port → LAN:port), which covers every
// real-world manual setup we've seen: routers expose port forwarding as
// a single port number, mapping the same port externally and internally.
// MapPort synthesises a Mapping with that single port, UnmapPort and
// StartRenewal are no-ops, and ExternalIP probes a public-IP discovery
// service since there's no UPnP gateway to ask.
//
// Constructed via NewManualForwarder when peer.Client detects the
// RADIANCE_PEER_EXTERNAL_PORT env var (or a future "manual port"
// setting). Use cases: routers with UPnP disabled (most common), users
// who deliberately turned UPnP off for security, ISP-provided gateways
// that ship without IGD, networks where the user has port-forwarded by
// hand because UPnP didn't work.
type ManualForwarder struct {
	port    uint16
	mapping *Mapping
}

// NewManualForwarder returns a ManualForwarder for the given TCP port,
// which it reports as both the external (WAN-side) and internal
// (LAN-side) port. Splitting them isn't supported — every manual
// router-config UI we've seen treats port forwarding as a single port
// number, and the 1:1 case is the only one that comes up in practice.
func NewManualForwarder(port uint16) (*ManualForwarder, error) {
	if port == 0 {
		return nil, fmt.Errorf("manual forwarder requires non-zero port")
	}
	return &ManualForwarder{port: port}, nil
}

// MapPort returns the manually-configured port as a synthetic Mapping.
// Ignores the caller-supplied internalPort — the manual forwarder
// already has its port fixed at construction. description is unused;
// real router configuration was done by the user out-of-band.
func (f *ManualForwarder) MapPort(_ context.Context, _ uint16, _ string) (*Mapping, error) {
	if f.mapping != nil {
		return nil, fmt.Errorf("manual forwarder already has an active mapping")
	}
	internalIP, err := localIP()
	if err != nil {
		return nil, fmt.Errorf("determine local ip: %w", err)
	}
	f.mapping = &Mapping{
		ExternalPort:  f.port,
		InternalPort:  f.port,
		InternalIP:    internalIP,
		Protocol:      "TCP",
		LeaseDuration: 0, // user-managed; no router-side TTL we own
		Method:        "manual",
	}
	return f.mapping, nil
}

func (f *ManualForwarder) UnmapPort(_ context.Context) error {
	if f == nil {
		return nil
	}
	// Nothing to undo — the user owns the router-side mapping. Just
	// drop our local handle so a subsequent MapPort doesn't error.
	f.mapping = nil
	return nil
}

// StartRenewal is a no-op — manual forwards persist on the router until
// the user removes them, with no UPnP lease to renew.
func (f *ManualForwarder) StartRenewal(_ context.Context) {}

// ExternalIP probes a public-IP discovery service (the radiance default
// methods, hitting api.iantem.io etc.) since there's no UPnP gateway to
// ask. Cached for the duration of the forwarder's life — the user's
// public IP shouldn't change mid-session, and a stale cache yields a
// clean re-register on a heartbeat 404 if it does.
func (f *ManualForwarder) ExternalIP(ctx context.Context) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result, err := publicip.Detect(probeCtx, &publicip.Config{
		Timeout:      5 * time.Second,
		MinConsensus: 1,
	})
	if err != nil {
		return "", fmt.Errorf("detect public ip: %w", err)
	}
	if result.IP == nil {
		return "", fmt.Errorf("public-ip detection returned no result")
	}
	return result.IP.String(), nil
}

// ParseManualPort is a small helper for callers that want to read a
// port from a string (env var, settings value). Returns 0, nil when s
// is empty so callers can use "no port configured" as the empty case
// without distinguishing it from a parse failure caller-side.
func ParseManualPort(s string) (uint16, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: %w", s, err)
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d out of range (1-65535)", n)
	}
	return uint16(n), nil
}

