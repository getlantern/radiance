//go:build !linux && !darwin && !windows

package vpn

import (
	"errors"

	"github.com/songgao/water"
)

// openTun creates a new TUN device with the given IP address.
func openTun(ip string) (*water.Interface, error) {
	return nil, errors.New("platform not supported")
}
