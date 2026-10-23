package tlsidentity

import (
	"crypto/x509"
	"errors"

	lbO "github.com/getlantern/lantern-box/option"
)

// CheckUnbounded rejects Unbounded options without a parseable egress CA and
// expected peer name. Other option types are accepted.
func CheckUnbounded(options any) error {
	var unbounded lbO.UnboundedOutboundOptions
	switch opts := options.(type) {
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
