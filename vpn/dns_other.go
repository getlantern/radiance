//go:build !linux

package vpn

import "errors"

func setSystemDNSServer(serverHost string) error {
	return errors.New("not implemented")
}

func restoreSystemDNSServer() {}
