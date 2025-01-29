package vpn

import "errors"

// Not implemented for Windows.
func startRouting(rConf *RoutingConfig, proxyAddr string, bypassUDP bool) error {
	return errors.New("not implemented")
}

// Not implemented for Windows.
func stopRouting(rConf *RoutingConfig) error {
	return errors.New("not implemented")
}
