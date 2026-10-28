package servers

import (
	"github.com/getlantern/radiance/internal/tlsidentity"
	"github.com/sagernet/sing-box/option"
)

// WithTLSVerification returns a copy with standard TLS verification enforced.
func (s *Server) WithTLSVerification() *Server {
	cp := s.Clone()
	switch opts := cp.Options.(type) {
	case option.Outbound:
		opts.Options = tlsidentity.Options(opts.Options)
		cp.Options = opts
	case option.Endpoint:
		opts.Options = tlsidentity.Options(opts.Options)
		cp.Options = opts
	}
	return cp
}
