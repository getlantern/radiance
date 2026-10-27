package tlsidentity

import (
	"crypto/x509"
	"errors"

	lbO "github.com/getlantern/lantern-box/option"
)

// CheckCustomOptions rejects Reflex and Unbounded options without supported peer authentication.
func CheckCustomOptions(options any) error {
	var unbounded lbO.UnboundedOutboundOptions
	switch opts := options.(type) {
	case lbO.ReflexOutboundOptions, *lbO.ReflexOutboundOptions:
		return errors.New("reflex does not support proxy authentication; use a protocol with a trusted certificate or public-key pin")
	case lbO.UnboundedOutboundOptions:
		unbounded = opts
	case *lbO.UnboundedOutboundOptions:
		if opts != nil {
			unbounded = *opts
		}
	default:
		return nil
	}
	if unbounded.EgressCA == "" {
		return errors.New("unbounded requires egress_ca for TLS authentication")
	}
	if unbounded.EgressServerName == "" {
		return errors.New("unbounded requires egress_server_name for TLS authentication")
	}
	if !x509.NewCertPool().AppendCertsFromPEM([]byte(unbounded.EgressCA)) {
		return errors.New("unbounded egress_ca must contain a PEM certificate")
	}
	return nil
}
