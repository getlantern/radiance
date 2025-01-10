//go:build !linux && !darwin && !windows

package vpn

import (
	"errors"

	"github.com/songgao/water"
)

// openTun creates a new TUN device with the given IP address and gateway.
func openTun(ip, gateway string) (*water.Interface, error) {
	return nil, errors.New("platform not supported")
}
