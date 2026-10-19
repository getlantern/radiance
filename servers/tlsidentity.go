package servers

import (
	"github.com/getlantern/radiance/internal/tlsidentity"
	"github.com/sagernet/sing-box/option"
)

// WithTLSVerification returns a copy whose standard outbound TLS options require peer authentication.
func (s *Server) WithTLSVerification() *Server {
	copy := s.Clone()
	switch opts := copy.Options.(type) {
	case option.Outbound:
		opts.Options = tlsidentity.Options(opts.Options)
		copy.Options = opts
	case option.Endpoint:
		opts.Options = tlsidentity.Options(opts.Options)
		copy.Options = opts
	}
	return copy
}
