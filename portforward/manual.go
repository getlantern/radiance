package portforward

import (
	"context"
	"fmt"
	"strconv"
)

// ManualForwarder exposes the same Map/Unmap/StartRenewal/ExternalIP
// surface as Forwarder but does no UPnP work. The user is expected to
// have configured a port forward on their router by hand (single-port
// 1:1 NAT — every consumer router exposes port forwarding as a single
// port number) and supplied the port number out-of-band.
//
// Use case: networks where UPnP is disabled or unavailable (router has
// UPnP off for security, ISP-provided gateways without IGD, networks
// behind double-NAT). The UPnP-based Forwarder fails in those
// environments; this type lets callers bypass discovery entirely.
type ManualForwarder struct {
	port uint16
}

// NewManualForwarder builds a ManualForwarder for a pre-configured
// router port forward. port must be in 1..65535; the caller is
// responsible for validating its input before calling.
func NewManualForwarder(port uint16) *ManualForwarder {
	return &ManualForwarder{port: port}
}

// ParseManualPort parses a string into a TCP port number. Values outside
// 1..65535 return an error so callers can log and fall through to UPnP
// discovery rather than register a non-listening port with the server.
func ParseManualPort(s string) (uint16, error) {
	p, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range (1..65535)", p)
	}
	return uint16(p), nil
}

// MapPort reports the configured port as both external and internal. The
// router-side rule is already in place; nothing to do at the protocol
// layer.
func (m *ManualForwarder) MapPort(_ context.Context, _ uint16, _ string) (*Mapping, error) {
	return &Mapping{
		ExternalPort: m.port,
		InternalPort: m.port,
		Method:       "manual",
	}, nil
}

// UnmapPort is a no-op: the user owns the router rule and is responsible
// for removing it.
func (m *ManualForwarder) UnmapPort(_ context.Context) error { return nil }

// StartRenewal is a no-op: manually-configured rules don't carry a UPnP
// lease and don't need refreshing.
func (m *ManualForwarder) StartRenewal(_ context.Context) {}

// ExternalIP returns the empty string deliberately. With a manual port
// forward we have no UPnP gateway to ask for the WAN address, and
// probing a public IP service from the client adds a network roundtrip
// for information lantern-cloud already has — the server observes the
// peer's source address on the register call and uses that as the
// canonical external IP when this field is empty.
func (m *ManualForwarder) ExternalIP(_ context.Context) (string, error) {
	return "", nil
}
