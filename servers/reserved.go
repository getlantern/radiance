package servers

import (
	"errors"
	"fmt"
	"slices"

	"github.com/getlantern/radiance/internal/redact"
)

// ErrReservedTag is returned for a user server whose tag the tunnel uses for
// its own outbounds and groups.
var ErrReservedTag = errors.New("tag is reserved")

// reservedTags are the tags the tunnel reserves: its selection groups, the
// personal proxies group and the built-in outbounds. vpn.ReservedTags must
// match it.
var reservedTags = []string{"auto", "manual", redact.PersonalProxiesTag, "direct", "block"}

// ReservedTags returns the tags no user server may use.
func ReservedTags() []string {
	return slices.Clone(reservedTags)
}

// checkUserTag refuses a reserved tag for a user (non-Lantern) server. Lantern
// config servers are exempt: a config may carry the built-in outbounds, which
// the tunnel merges but never offers.
func checkUserTag(srv *Server) error {
	if !srv.IsLantern && slices.Contains(reservedTags, srv.Tag) {
		return fmt.Errorf("server %q: %w", srv.Tag, ErrReservedTag)
	}
	return nil
}
