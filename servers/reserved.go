package servers

import (
	"errors"
	"fmt"
	"slices"

	"github.com/sagernet/sing-box/option"

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

// ErrTagMismatch is returned for a user server whose tag differs from the tag
// of the outbound or endpoint it carries, which is the tag the tunnel uses.
var ErrTagMismatch = errors.New("server tag does not match its outbound or endpoint tag")

// checkUserTag refuses a user (non-Lantern) server whose tag is reserved or
// differs from the tag of the outbound or endpoint it carries: the tunnel
// creates the outbound under the embedded tag, so a mismatch could slip a
// reserved tag past this check and replace one of the tunnel's own groups.
// Lantern config servers are exempt: a config may carry the built-in
// outbounds, which the tunnel merges but never offers.
func checkUserTag(srv *Server) error {
	if srv.IsLantern {
		return nil
	}
	if embedded, ok := embeddedTag(srv.Options); ok && embedded != srv.Tag {
		return fmt.Errorf("server %q carries %q: %w", srv.Tag, embedded, ErrTagMismatch)
	}
	if slices.Contains(reservedTags, srv.Tag) {
		return fmt.Errorf("server %q: %w", srv.Tag, ErrReservedTag)
	}
	return nil
}

// embeddedTag returns the tag of the outbound or endpoint in opts.
func embeddedTag(opts any) (string, bool) {
	switch o := opts.(type) {
	case option.Outbound:
		return o.Tag, true
	case *option.Outbound:
		if o != nil {
			return o.Tag, true
		}
	case option.Endpoint:
		return o.Tag, true
	case *option.Endpoint:
		if o != nil {
			return o.Tag, true
		}
	}
	return "", false
}
