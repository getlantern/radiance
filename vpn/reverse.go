package vpn

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sagernet/sing-box/adapter"
	O "github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/service"

	lbA "github.com/getlantern/lantern-box/adapter"
	lbC "github.com/getlantern/lantern-box/constant"
	lbO "github.com/getlantern/lantern-box/option"
	"github.com/getlantern/lantern-box/protocol/samizdatreverse"

	"github.com/getlantern/radiance/servers"
)

// reversePortalTag is the portal inbound's tag, which every reverse outbound
// names to find its sessions.
const reversePortalTag = "samizdat-portal"

type reversePortal interface {
	Configure(samizdatreverse.PortalConfig) error
	Rebind() error
}

// setReversePortal configures the portal, creating it first if need be, or
// with a nil cfg removes it and every reverse outbound.
func (t *tunnel) setReversePortal(cfg *samizdatreverse.PortalConfig) error {
	t.outboundMu.Lock()
	defer t.outboundMu.Unlock()
	if cfg == nil {
		err := t.setReverseOutboundsLocked(nil)
		if errors.Is(err, errLibboxClosed) {
			return err
		}
		return errors.Join(err, t.removeReversePortalLocked())
	}
	portal, err := t.reversePortalLocked()
	if err != nil {
		return err
	}
	return portal.Configure(*cfg)
}

// reversePortalLocked returns the tunnel's portal, creating it on first use.
// The caller must hold t.outboundMu.
func (t *tunnel) reversePortalLocked() (reversePortal, error) {
	inbounds := service.FromContext[adapter.InboundManager](t.ctx)
	if inbounds == nil {
		return nil, errors.New("inbound manager not found")
	}
	if _, ok := inbounds.Get(reversePortalTag); !ok {
		router := service.FromContext[adapter.Router](t.ctx)
		logger := t.logFactory.NewLogger("inbound/" + reversePortalTag + "[" + lbC.TypeSamizdatPortal + "]")
		err := inbounds.Create(t.ctx, router, logger, reversePortalTag, lbC.TypeSamizdatPortal,
			&lbO.SamizdatPortalInboundOptions{})
		if err != nil {
			return nil, fmt.Errorf("create reverse portal: %w", err)
		}
	}
	in, _ := inbounds.Get(reversePortalTag)
	portal, ok := in.(reversePortal)
	if !ok {
		return nil, errors.New("reverse portal cannot be configured")
	}
	return portal, nil
}

// removeReversePortalLocked closes and removes the portal, if any. The caller
// must hold t.outboundMu.
func (t *tunnel) removeReversePortalLocked() error {
	inbounds := service.FromContext[adapter.InboundManager](t.ctx)
	if inbounds == nil {
		return nil
	}
	if _, ok := inbounds.Get(reversePortalTag); !ok {
		return nil
	}
	if err := inbounds.Remove(reversePortalTag); err != nil {
		return fmt.Errorf("remove reverse portal: %w", err)
	}
	return nil
}

func (t *tunnel) rebindReversePortal() error {
	t.outboundMu.Lock()
	defer t.outboundMu.Unlock()
	inbounds := service.FromContext[adapter.InboundManager](t.ctx)
	if inbounds == nil {
		return nil
	}
	in, ok := inbounds.Get(reversePortalTag)
	if !ok {
		return nil
	}
	portal, ok := in.(reversePortal)
	if !ok {
		return errors.New("reverse portal cannot be rebound")
	}
	return portal.Rebind()
}

func (t *tunnel) setReverseOutbounds(tags []string) error {
	t.outboundMu.Lock()
	defer t.outboundMu.Unlock()
	return t.setReverseOutboundsLocked(tags)
}

// setReverseOutboundsLocked makes the selection groups' reverse outbounds
// exactly tags, which must all be reverse tags, and resyncs the personal proxy
// groups. The caller must hold t.outboundMu.
func (t *tunnel) setReverseOutboundsLocked(tags []string) error {
	if t.mutGrpMgr == nil {
		return nil
	}
	selector, ok := t.mutGrpMgr.OutboundGroup(ManualSelectTag)
	if !ok {
		return errors.New("manual select group not found")
	}
	var stale []string
	for _, tag := range selector.All() {
		if lbA.IsReverseHelperTag(tag) && !slices.Contains(tags, tag) {
			stale = append(stale, tag)
		}
	}
	list := servers.ServerList{Servers: make([]*servers.Server, 0, len(tags))}
	for _, tag := range tags {
		list.Servers = append(list.Servers, reverseServer(tag))
	}
	err := t.loadOutboundsLocked(list, false)
	if errors.Is(err, errLibboxClosed) {
		return err
	}
	errs := []error{err}
	if len(stale) > 0 {
		err := t.removeOutboundsLocked(stale)
		if errors.Is(err, errLibboxClosed) {
			return err
		}
		errs = append(errs, err)
	}
	errs = append(errs, t.syncPersonalProxiesLocked())
	return errors.Join(errs...)
}

// reverseServer is the reverse outbound for tag. It is not a Lantern server:
// the helper's bridge at the far end does not read client info.
func reverseServer(tag string) *servers.Server {
	return &servers.Server{
		Tag:  tag,
		Type: lbC.TypeSamizdatReverse,
		Options: O.Outbound{
			Tag:     tag,
			Type:    lbC.TypeSamizdatReverse,
			Options: &lbO.SamizdatReverseOutboundOptions{Portal: reversePortalTag},
		},
	}
}
