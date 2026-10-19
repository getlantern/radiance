// Package tlsidentity normalizes outbound TLS options to require peer authentication.
package tlsidentity

import (
	"reflect"

	lbO "github.com/getlantern/lantern-box/option"
	"github.com/sagernet/sing-box/option"
)

// Options disables unauthenticated bypasses in outbound TLS options without modifying the input.
func Options(options any) any {
	v := reflect.ValueOf(options)
	if !v.IsValid() {
		return options
	}
	isPointer := v.Kind() == reflect.Pointer
	if isPointer {
		if v.IsNil() {
			return options
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return options
	}
	copy := reflect.New(v.Type())
	copy.Elem().Set(v)
	switch normalized := copy.Interface().(type) {
	case *lbO.UnboundedOutboundOptions:
		normalized.InsecureDoNotVerifyClientCert = false
		normalized.InsecureDoNotVerifyDiscoveryCert = false
	case option.OutboundTLSOptionsWrapper:
		tls := normalized.TakeOutboundTLSOptions()
		if tls == nil || !tls.Insecure || len(tls.CertificatePublicKeySHA256) > 0 || (tls.Reality != nil && tls.Reality.Enabled) {
			return options
		}
		verifiedTLS := *tls
		verifiedTLS.Insecure = false
		normalized.ReplaceOutboundTLSOptions(&verifiedTLS)
	default:
		return options
	}
	if isPointer {
		return copy.Interface()
	}
	return copy.Elem().Interface()
}
