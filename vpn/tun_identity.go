package vpn

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sagernet/sing-box/option"
)

// maxTUNIdentityRetries bounds the alternate adapter identities tried after a
// collision. A host holding more orphans than this needs the devnodes removed.
const maxTUNIdentityRetries = 3

// isTUNIdentityCollision reports whether err is Windows refusing to create the
// Wintun adapter because an orphaned devnode already claims its identity.
//
// sing-tun derives the adapter GUID from md5("wintun"+name) and sing-box picks
// that name by scanning *present* interfaces, which an orphaned devnode is not.
// So CreateAdapter fails ERROR_ALREADY_EXISTS, OpenAdapter fails
// ERROR_NOT_FOUND, and every attempt recomputes the same colliding identity.
//
// The signature is Windows-only by construction: "create adapter" is wrapped in
// sing-tun's tun_windows.go. Deliberately narrow — an unrelated os.ErrExist in
// bring-up must not mutate the adapter identity, and a device that was created
// but failed to enable (ERROR_SET_NOT_FOUND, engineering#3854) is not an
// existence error, so those hosts never pay for a retry here.
func isTUNIdentityCollision(err error) bool {
	if err == nil || !errors.Is(err, os.ErrExist) {
		return false
	}
	return strings.Contains(err.Error(), "create adapter")
}

// tunIdentityName returns the adapter name for the given retry. sing-box's own
// naming starts at tun0 and cannot see the orphan, so step past it explicitly.
func tunIdentityName(attempt int) string {
	return fmt.Sprintf("tun%d", attempt)
}

// setTUNInterfaceName pins the TUN inbound's adapter name, which is what the
// Wintun GUID is derived from. False when options carry no TUN inbound.
func setTUNInterfaceName(options option.Options, name string) bool {
	for _, inbound := range options.Inbounds {
		if inbound.Tag != inboundTag {
			continue
		}
		tunOpts, ok := inbound.Options.(*option.TunInboundOptions)
		if !ok {
			return false
		}
		tunOpts.InterfaceName = name
		return true
	}
	return false
}
